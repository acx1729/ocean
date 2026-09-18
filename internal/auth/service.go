package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/did"
)

// RefreshCookie is the httpOnly cookie carrying the refresh token for browsers.
const RefreshCookie = "kb_refresh"

// refreshCookiePath scopes the cookie to the auth service, the only place that reads it.
const refreshCookiePath = "/rpc/kb.v1.AuthService/"

// Service implements kb.v1.AuthService.
type Service struct {
	cfg     *config.Config
	store   *Store
	tokens  *TokenIssuer
	eip1271 *EIP1271
	clock   Clock
	log     *slog.Logger
}

var _ kbv1connect.AuthServiceHandler = (*Service)(nil)

// NewService builds the auth service.
func NewService(cfg *config.Config, store *Store, tokens *TokenIssuer, clock Clock, log *slog.Logger) *Service {
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Service{cfg: cfg, store: store, tokens: tokens, clock: clock, log: log}
	if cfg.EVMRPCURL != "" {
		s.eip1271 = &EIP1271{RPCURL: cfg.EVMRPCURL}
	}
	return s
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Challenge issues a nonce and the message to sign.
func (s *Service) Challenge(ctx context.Context, req *connect.Request[kbv1.ChallengeRequest]) (*connect.Response[kbv1.ChallengeResponse], error) {
	didStr := did.Normalize(strings.TrimSpace(req.Msg.GetDid()))
	if err := did.Validate(didStr); err != nil {
		return nil, apierr.InvalidArgument("did", err.Error())
	}
	now := s.clock()
	expires := now.Add(challengeTTL)
	nonce, err := newNonce()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	var message string
	switch did.KindOf(didStr) {
	case did.KindPKH:
		p, _ := did.ParsePKH(didStr)
		message = SIWEMessage{
			Domain: s.cfg.PublicHost(), Address: ChecksumAddress(p.Address), URI: s.cfg.PublicURL, Version: "1",
			ChainID: p.ChainID, Nonce: nonce, IssuedAt: now, ExpirationTime: expires,
		}.String()
	default:
		if _, err := did.Ed25519PublicKey(didStr); err != nil {
			return nil, apierr.InvalidArgument("did", "did:key must carry an Ed25519 public key")
		}
		message = KeyMessage(s.cfg.PublicHost(), didStr, s.cfg.PublicURL, nonce, now, expires)
	}
	if err := s.store.CreateChallenge(ctx, nonce, didStr, message, expires); err != nil {
		return nil, apierr.Internal(err)
	}
	return connect.NewResponse(&kbv1.ChallengeResponse{Nonce: nonce, Message: message, ExpiresAt: timestamppb.New(expires)}), nil
}

// Verify checks the signed challenge and opens a session.
func (s *Service) Verify(ctx context.Context, req *connect.Request[kbv1.VerifyRequest]) (*connect.Response[kbv1.VerifyResponse], error) {
	didStr := did.Normalize(strings.TrimSpace(req.Msg.GetDid()))
	if err := did.Validate(didStr); err != nil {
		return nil, apierr.InvalidArgument("did", err.Error())
	}
	now := s.clock()
	locked, err := s.store.LockedOut(ctx, didStr, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if locked {
		return nil, apierr.ResourceExhausted("verify", "too many failed attempts; try again later")
	}
	message, err := s.store.ConsumeChallenge(ctx, req.Msg.GetNonce(), didStr, now)
	if err != nil {
		if errors.Is(err, ErrChallengeExpired) {
			return nil, apierr.Unauthenticated("challenge expired, unknown or already used")
		}
		return nil, apierr.Internal(err)
	}
	fail := func(reason string) error {
		_ = s.store.RecordFailure(ctx, didStr, now)
		return apierr.Unauthenticated(reason)
	}
	if m := req.Msg.GetMessage(); m != "" && m != message {
		return nil, fail("signed message does not match the challenge")
	}
	if err := s.verifySignature(ctx, didStr, message, req.Msg.GetSignature(), now); err != nil {
		if errors.Is(err, ErrContractWallet) {
			return nil, apierr.FailedPrecondition("contract wallets are not accepted: the node has no KB_EVM_RPC_URL")
		}
		return nil, fail("invalid signature")
	}
	principal, err := s.store.EnsureUser(ctx, didStr)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if principal.DisabledAt != nil {
		return nil, apierr.PermissionDenied("sign_in", didStr, false)
	}
	deviceDID := ""
	if dev := req.Msg.GetDevice(); dev != nil && dev.GetDid() != "" {
		if err := s.registerDevice(ctx, didStr, dev, nil); err != nil {
			return nil, err
		}
		deviceDID = dev.GetDid()
	}
	res, err := s.openSession(ctx, principal.ID, deviceDID, KindUser, now)
	if err != nil {
		return nil, err
	}
	out := connect.NewResponse(&kbv1.VerifyResponse{
		AccessToken: res.access, RefreshToken: res.refresh,
		AccessExpiresAt: timestamppb.New(res.accessExp), RefreshExpiresAt: timestamppb.New(res.refreshExp),
		Principal: toPrincipal(principal),
	})
	s.setRefreshCookie(req.Header(), out.Header(), res.refresh, res.refreshExp)
	return out, nil
}

func (s *Service) verifySignature(ctx context.Context, didStr, message string, sig []byte, now time.Time) error {
	switch did.KindOf(didStr) {
	case did.KindKey:
		return VerifyEd25519(didStr, []byte(message), sig)
	case did.KindPKH:
		p, err := did.ParsePKH(didStr)
		if err != nil {
			return err
		}
		m, err := ParseSIWE(message)
		if err != nil {
			return err
		}
		if !strings.EqualFold(m.Domain, s.cfg.PublicHost()) || m.ChainID != p.ChainID || !strings.EqualFold(m.Address, p.Address) {
			return ErrInvalidSignature
		}
		if !m.ExpirationTime.IsZero() && !m.ExpirationTime.After(now) {
			return ErrChallengeExpired
		}
		hash := EIP191Hash([]byte(message))
		addr, err := RecoverAddress(hash, sig)
		if err == nil && strings.EqualFold(addr, p.Address) {
			return nil
		}
		// Not an EOA signature: try EIP-1271 against the account contract.
		return s.eip1271.IsValidSignature(ctx, p.Address, hash, sig)
	}
	return ErrInvalidSignature
}

type sessionResult struct {
	access, refresh       string
	accessExp, refreshExp time.Time
	sessionID             string
}

func (s *Service) openSession(ctx context.Context, principal, deviceDID string, kind Kind, now time.Time) (*sessionResult, error) {
	refresh, err := NewOpaqueToken(RefreshPrefix)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	refreshExp := now.Add(s.cfg.RefreshTTL)
	sid, err := s.store.CreateSession(ctx, principal, deviceDID, HashToken(refresh), refreshExp, s.cfg.Limits.SessionsPerUser, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	access, claims, err := s.tokens.Issue(Claims{Subject: principal, Owner: principal, Kind: kind, SessionID: sid, DeviceID: deviceDID}, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	return &sessionResult{access: access, refresh: refresh, accessExp: claims.ExpiresAt, refreshExp: refreshExp, sessionID: sid}, nil
}

// isWebClient reports whether the caller asked for cookie handling.
func isWebClient(h http.Header) bool { return h.Get("X-KB-Client") == "web" }

func (s *Service) setRefreshCookie(reqHeader, resHeader http.Header, refresh string, exp time.Time) {
	if !isWebClient(reqHeader) {
		return
	}
	c := &http.Cookie{
		Name: RefreshCookie, Value: refresh, Path: refreshCookiePath, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.Secure(), Expires: exp,
	}
	resHeader.Add("Set-Cookie", c.String())
}

func (s *Service) clearRefreshCookie(reqHeader, resHeader http.Header) {
	if !isWebClient(reqHeader) {
		return
	}
	c := &http.Cookie{Name: RefreshCookie, Value: "", Path: refreshCookiePath, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.cfg.Secure(), MaxAge: -1}
	resHeader.Add("Set-Cookie", c.String())
}

func cookieRefresh(h http.Header) string {
	r := &http.Request{Header: h}
	c, err := r.Cookie(RefreshCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// Refresh rotates the refresh token and mints a new access token.
func (s *Service) Refresh(ctx context.Context, req *connect.Request[kbv1.RefreshRequest]) (*connect.Response[kbv1.RefreshResponse], error) {
	token := req.Msg.GetRefreshToken()
	if token == "" && isWebClient(req.Header()) {
		token = cookieRefresh(req.Header())
	}
	if token == "" {
		return nil, apierr.Unauthenticated("refresh token required")
	}
	now := s.clock()
	next, err := NewOpaqueToken(RefreshPrefix)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	sess, err := s.store.RotateRefresh(ctx, HashToken(token), HashToken(next), now)
	if err != nil {
		switch {
		case errors.Is(err, ErrRefreshReused):
			s.log.Warn("refresh token reuse detected; session revoked")
			return nil, apierr.Unauthenticated("refresh token reuse detected; session revoked")
		case errors.Is(err, ErrUnauthenticated):
			return nil, apierr.Unauthenticated("invalid or expired refresh token")
		}
		return nil, apierr.Internal(err)
	}
	principal, err := s.store.GetPrincipal(ctx, sess.PrincipalID)
	if err != nil || principal.DisabledAt != nil {
		return nil, apierr.Unauthenticated("principal disabled")
	}
	kind := KindUser
	if principal.Kind == KindLink {
		kind = KindLink
	}
	owner := principal.ID
	if principal.OwnerID != "" {
		owner = principal.OwnerID
	}
	access, claims, err := s.tokens.Issue(Claims{Subject: principal.ID, Owner: owner, Kind: kind, SessionID: sess.ID, DeviceID: sess.DeviceID}, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := connect.NewResponse(&kbv1.RefreshResponse{
		AccessToken: access, RefreshToken: next,
		AccessExpiresAt: timestamppb.New(claims.ExpiresAt), RefreshExpiresAt: timestamppb.New(sess.ExpiresAt),
	})
	s.setRefreshCookie(req.Header(), out.Header(), next, sess.ExpiresAt)
	return out, nil
}

// Revoke ends the current session, the session of a refresh token, or all sessions.
func (s *Service) Revoke(ctx context.Context, req *connect.Request[kbv1.RevokeRequest]) (*connect.Response[kbv1.RevokeResponse], error) {
	now := s.clock()
	id, authed := FromContext(ctx)
	token := req.Msg.GetRefreshToken()
	if token == "" && isWebClient(req.Header()) {
		token = cookieRefresh(req.Header())
	}
	switch {
	case req.Msg.GetAll():
		if !authed {
			return nil, apierr.Unauthenticated("")
		}
		if err := s.store.RevokeAllSessions(ctx, id.EffectiveOwner(), now); err != nil {
			return nil, apierr.Internal(err)
		}
	case token != "":
		if err := s.store.RevokeSessionByRefresh(ctx, HashToken(token), now); err != nil {
			return nil, apierr.Internal(err)
		}
	case authed && id.SessionID != "":
		if err := s.store.RevokeSession(ctx, id.SessionID, now); err != nil {
			return nil, apierr.Internal(err)
		}
	default:
		return nil, apierr.Unauthenticated("nothing to revoke")
	}
	out := connect.NewResponse(&kbv1.RevokeResponse{})
	s.clearRefreshCookie(req.Header(), out.Header())
	return out, nil
}

func (s *Service) registerDevice(ctx context.Context, owner string, dev *kbv1.DeviceRegistration, pubkey []byte) error {
	if err := did.Validate(dev.GetDid()); err != nil || did.KindOf(dev.GetDid()) != did.KindKey {
		return apierr.InvalidArgument("device.did", "device must be a did:key")
	}
	if pubkey == nil {
		pk, err := did.Ed25519PublicKey(dev.GetDid())
		if err != nil {
			return apierr.InvalidArgument("device.did", err.Error())
		}
		pubkey = pk
	}
	msg := []byte(attestationMsg + dev.GetDid())
	var err error
	switch did.KindOf(owner) {
	case did.KindKey:
		err = VerifyEd25519(owner, msg, dev.GetAttestation())
	case did.KindPKH:
		p, _ := did.ParsePKH(owner)
		addr, rerr := RecoverAddress(EIP191Hash(msg), dev.GetAttestation())
		if rerr != nil || !strings.EqualFold(addr, p.Address) {
			err = ErrInvalidSignature
		}
	}
	if err != nil {
		return apierr.InvalidArgument("device.attestation", "attestation is not a valid owner signature over the device did")
	}
	if err := s.store.RegisterDevice(ctx, owner, dev.GetDid(), dev.GetDisplayName(), pubkey, dev.GetAttestation()); err != nil {
		return apierr.Internal(err)
	}
	return nil
}

// RegisterDevice attaches a device key to the caller.
func (s *Service) RegisterDevice(ctx context.Context, req *connect.Request[kbv1.RegisterDeviceRequest]) (*connect.Response[kbv1.RegisterDeviceResponse], error) {
	id, err := MustIdentity(ctx)
	if err != nil || id.Kind != KindUser {
		return nil, apierr.Unauthenticated("a user session is required")
	}
	if req.Msg.GetDevice() == nil {
		return nil, apierr.InvalidArgument("device", "required")
	}
	if err := s.registerDevice(ctx, id.Principal, req.Msg.GetDevice(), req.Msg.GetSigningPubkey()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.RegisterDeviceResponse{Device: &kbv1.Device{Did: req.Msg.GetDevice().GetDid(), DisplayName: req.Msg.GetDevice().GetDisplayName(), CreatedAt: timestamppb.New(s.clock())}}), nil
}

// ListDevices lists the caller's devices.
func (s *Service) ListDevices(ctx context.Context, req *connect.Request[kbv1.ListDevicesRequest]) (*connect.Response[kbv1.ListDevicesResponse], error) {
	id, err := MustIdentity(ctx)
	if err != nil {
		return nil, apierr.Unauthenticated("")
	}
	devs, err := s.store.ListDevices(ctx, id.EffectiveOwner())
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := &kbv1.ListDevicesResponse{}
	for _, d := range devs {
		out.Devices = append(out.Devices, &kbv1.Device{Did: d.DID, DisplayName: d.DisplayName, CreatedAt: timestamppb.New(d.CreatedAt), LastSeenAt: tsOrNil(d.LastSeenAt), RevokedAt: tsOrNil(d.RevokedAt)})
	}
	return connect.NewResponse(out), nil
}

// RevokeDevice disables a device and its sessions.
func (s *Service) RevokeDevice(ctx context.Context, req *connect.Request[kbv1.RevokeDeviceRequest]) (*connect.Response[kbv1.RevokeDeviceResponse], error) {
	id, err := MustIdentity(ctx)
	if err != nil || id.Kind != KindUser {
		return nil, apierr.Unauthenticated("a user session is required")
	}
	if err := s.store.RevokeDevice(ctx, id.Principal, req.Msg.GetDid(), s.clock()); err != nil {
		if errors.Is(err, db.ErrNoRows) {
			return nil, apierr.NotFound("device", req.Msg.GetDid())
		}
		return nil, apierr.Internal(err)
	}
	return connect.NewResponse(&kbv1.RevokeDeviceResponse{}), nil
}

// Me describes the caller.
func (s *Service) Me(ctx context.Context, req *connect.Request[kbv1.MeRequest]) (*connect.Response[kbv1.MeResponse], error) {
	id, err := MustIdentity(ctx)
	if err != nil {
		return nil, apierr.Unauthenticated("")
	}
	out := &kbv1.MeResponse{DeviceDid: id.DeviceID, SessionId: id.SessionID}
	if id.Operator {
		out.Principal = &kbv1.Principal{Did: "node", Kind: kbv1.PrincipalKind_PRINCIPAL_KIND_NODE}
		return connect.NewResponse(out), nil
	}
	p, err := s.store.GetPrincipal(ctx, id.Principal)
	if err != nil {
		if errors.Is(err, db.ErrNoRows) {
			return nil, apierr.Unauthenticated("unknown principal")
		}
		return nil, apierr.Internal(err)
	}
	out.Principal = toPrincipal(p)
	if id.Owner != "" && id.Owner != id.Principal {
		if o, err := s.store.GetPrincipal(ctx, id.Owner); err == nil {
			out.Owner = toPrincipal(o)
		}
	}
	ms, err := s.store.Memberships(ctx, id.EffectiveOwner())
	if err != nil {
		return nil, apierr.Internal(err)
	}
	for _, m := range ms {
		if id.IsAgent() && id.WorkspaceID != m.WorkspaceID {
			continue
		}
		out.Memberships = append(out.Memberships, &kbv1.Membership{Workspace: &kbv1.Workspace{Id: m.WorkspaceID, Slug: m.Slug, Name: m.Name, Role: m.Role}, Role: m.Role})
	}
	return connect.NewResponse(out), nil
}

func toPrincipal(p *Principal) *kbv1.Principal {
	return &kbv1.Principal{Did: p.ID, Kind: principalKind(p.Kind), OwnerDid: p.OwnerID, DisplayName: p.DisplayName, DisabledAt: tsOrNil(p.DisabledAt), CreatedAt: timestamppb.New(p.CreatedAt)}
}

func principalKind(k Kind) kbv1.PrincipalKind {
	switch k {
	case KindUser:
		return kbv1.PrincipalKind_PRINCIPAL_KIND_USER
	case KindDevice:
		return kbv1.PrincipalKind_PRINCIPAL_KIND_DEVICE
	case KindAgent:
		return kbv1.PrincipalKind_PRINCIPAL_KIND_AGENT
	case KindLink:
		return kbv1.PrincipalKind_PRINCIPAL_KIND_LINK
	case KindNode:
		return kbv1.PrincipalKind_PRINCIPAL_KIND_NODE
	}
	return kbv1.PrincipalKind_PRINCIPAL_KIND_UNSPECIFIED
}

func tsOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
