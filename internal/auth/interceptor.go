package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
)

// Authenticator resolves bearer tokens to identities and enforces rate limits.
// It serves both the Connect interceptor and the non-Connect routes
// (WebSocket sync, MCP, uploads).
type Authenticator struct {
	tokens        *TokenIssuer
	store         *Store
	limiter       Limiter
	limits        config.Limits
	operatorToken string
	trustProxy    bool
	clock         Clock
	log           *slog.Logger

	mu    sync.Mutex
	cache map[string]agentCacheEntry
}

type agentCacheEntry struct {
	tok     *AgentToken
	at      time.Time
	touched time.Time
}

// AuthenticatorOptions configure the authenticator.
type AuthenticatorOptions struct {
	Tokens        *TokenIssuer
	Store         *Store
	Limiter       Limiter
	Limits        config.Limits
	OperatorToken string
	TrustProxy    bool
	Clock         Clock
	Logger        *slog.Logger
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(o AuthenticatorOptions) *Authenticator {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Authenticator{tokens: o.Tokens, store: o.Store, limiter: o.Limiter, limits: o.Limits, operatorToken: o.OperatorToken, trustProxy: o.TrustProxy, clock: o.Clock, log: o.Logger, cache: map[string]agentCacheEntry{}}
}

// Resolve authenticates a bearer token.
func (a *Authenticator) Resolve(ctx context.Context, token string) (*Identity, error) {
	now := a.clock()
	switch TokenKind(token) {
	case KindOperator:
		if a.operatorToken != "" && ConstantTimeEqual(token, a.operatorToken) {
			return &Identity{Principal: "node", Kind: KindOperator, Operator: true}, nil
		}
		return nil, ErrUnauthenticated
	case KindAgent:
		return a.resolveAgent(ctx, token, now)
	case KindUser:
		c, err := a.tokens.Verify(token, now)
		if err != nil {
			return nil, err
		}
		return &Identity{Principal: c.Subject, Kind: c.Kind, Owner: c.Owner, SessionID: c.SessionID, DeviceID: c.DeviceID, Anonymous: c.Anonymous, WorkspaceID: c.WorkspaceID, ExpiresAt: c.ExpiresAt}, nil
	}
	return nil, ErrUnauthenticated
}

func (a *Authenticator) resolveAgent(ctx context.Context, token string, now time.Time) (*Identity, error) {
	hash := HashToken(token)
	key := string(hash)
	a.mu.Lock()
	e, ok := a.cache[key]
	a.mu.Unlock()
	if !ok || now.Sub(e.at) > 30*time.Second {
		tok, err := a.store.AgentTokenByHash(ctx, hash)
		if err != nil {
			if errors.Is(err, db.ErrNoRows) {
				return nil, ErrUnauthenticated
			}
			return nil, err
		}
		e = agentCacheEntry{tok: tok, at: now}
		a.mu.Lock()
		if len(a.cache) > 10000 {
			a.cache = map[string]agentCacheEntry{}
		}
		a.cache[key] = e
		a.mu.Unlock()
	}
	tok := e.tok
	if tok.RevokedAt != nil {
		return nil, ErrUnauthenticated
	}
	if tok.ExpiresAt != nil && !tok.ExpiresAt.After(now) {
		return nil, ErrTokenExpired
	}
	if tok.ThrottledUntil != nil && tok.ThrottledUntil.After(now) {
		return nil, ErrRateLimited
	}
	if now.Sub(e.touched) > time.Minute {
		a.mu.Lock()
		e.touched = now
		a.cache[key] = e
		a.mu.Unlock()
		go func() {
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := a.store.TouchAgentToken(tctx, tok.WorkspaceID, tok.ID, now); err != nil {
				a.log.Warn("touch agent token", "error", err)
			}
		}()
	}
	return &Identity{
		Principal: tok.PrincipalID, Kind: KindAgent, Owner: tok.OwnerID,
		AgentTokenID: tok.ID, WorkspaceID: tok.WorkspaceID, Scopes: tok.Scopes, Tools: tok.Tools,
	}, nil
}

// Invalidate drops a cached agent token (after revocation).
func (a *Authenticator) Invalidate(tokenHash []byte) {
	a.mu.Lock()
	delete(a.cache, string(tokenHash))
	a.mu.Unlock()
}

// AuthenticateRequest resolves the identity of a plain HTTP request from its
// Authorization header.
func (a *Authenticator) AuthenticateRequest(r *http.Request) (*Identity, error) {
	tok, err := ParseBearer(r.Header.Get("Authorization"))
	if err != nil {
		return nil, ErrUnauthenticated
	}
	return a.Resolve(r.Context(), tok)
}

// Limit applies the per-identity request limit and returns the outcome.
func (a *Authenticator) Limit(ctx context.Context, id *Identity) (LimitResult, error) {
	if a.limiter == nil || id == nil || id.Operator {
		return LimitResult{Allowed: true}, nil
	}
	switch id.Kind {
	case KindAgent:
		return a.limiter.Allow(ctx, "agent:"+id.AgentTokenID, a.limits.RateAgentPerMinute, time.Minute)
	default:
		bucket := "sess:" + id.SessionID
		if id.SessionID == "" {
			bucket = "principal:" + id.Principal
		}
		return a.limiter.Allow(ctx, bucket, a.limits.RateAccessPerMinute, time.Minute)
	}
}

// LimitIP applies the per-IP challenge limit.
func (a *Authenticator) LimitIP(ctx context.Context, ip string) (LimitResult, error) {
	if a.limiter == nil || ip == "" {
		return LimitResult{Allowed: true}, nil
	}
	return a.limiter.Allow(ctx, "ip:"+ip, a.limits.RateChallengePerMin, time.Minute)
}

// ClientIP extracts the caller address, honoring X-Forwarded-For only when
// the node is configured to trust its proxy.
func (a *Authenticator) ClientIP(header http.Header, peerAddr string) string {
	if a.trustProxy {
		if xff := header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(peerAddr)
	if err != nil {
		return peerAddr
	}
	return host
}

// Public procedures need no credentials.
var publicProcedures = map[string]bool{
	"/kb.v1.AuthService/Challenge":    true,
	"/kb.v1.AuthService/Verify":       true,
	"/kb.v1.AuthService/Refresh":      true,
	"/kb.v1.AuthService/Revoke":       true,
	"/kb.v1.ShareLinksService/Redeem": true,
	"/kb.v1.AdminService/Health":      true,
}

// IsPublic reports whether a Connect procedure is reachable without a token.
func IsPublic(procedure string) bool { return publicProcedures[procedure] }

// Interceptor is the Connect interceptor: it authenticates every non-public
// call, applies rate limits and attaches the identity to the context.
type Interceptor struct {
	a *Authenticator
}

// NewInterceptor wraps an Authenticator.
func NewInterceptor(a *Authenticator) *Interceptor { return &Interceptor{a: a} }

func setLimitHeaders(h http.Header, r LimitResult) {
	if r.Limit == 0 {
		return
	}
	h.Set("RateLimit-Limit", strconv.Itoa(r.Limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(r.Remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(int(r.ResetAfter.Seconds())+1))
}

func (i *Interceptor) authenticate(ctx context.Context, procedure string, header http.Header, peer string) (context.Context, LimitResult, error) {
	if IsPublic(procedure) {
		if strings.HasPrefix(procedure, "/kb.v1.AuthService/") {
			lr, err := i.a.LimitIP(ctx, i.a.ClientIP(header, peer))
			if err != nil {
				i.a.log.Warn("rate limiter", "error", err)
			} else if !lr.Allowed {
				return ctx, lr, apierr.ResourceExhausted("ip", "too many authentication requests")
			}
		}
		// An optional token still identifies the caller (Revoke of the current session).
		if h := header.Get("Authorization"); h != "" {
			if tok, err := ParseBearer(h); err == nil {
				if id, err := i.a.Resolve(ctx, tok); err == nil {
					ctx = WithIdentity(ctx, id)
				}
			}
		}
		return ctx, LimitResult{}, nil
	}
	tok, err := ParseBearer(header.Get("Authorization"))
	if err != nil {
		return ctx, LimitResult{}, apierr.Unauthenticated("")
	}
	id, err := i.a.Resolve(ctx, tok)
	if err != nil {
		switch {
		case errors.Is(err, ErrTokenExpired):
			return ctx, LimitResult{}, apierr.Unauthenticated("token expired")
		case errors.Is(err, ErrRateLimited):
			return ctx, LimitResult{}, apierr.ResourceExhausted("agent", "token throttled after repeated failures")
		case errors.Is(err, ErrUnauthenticated):
			return ctx, LimitResult{}, apierr.Unauthenticated("invalid token")
		}
		i.a.log.Error("resolve identity", "error", err)
		return ctx, LimitResult{}, apierr.Internal(err)
	}
	if id.Operator && !strings.HasPrefix(procedure, "/kb.v1.AdminService/") {
		return ctx, LimitResult{}, apierr.PermissionDenied("operator", procedure, false)
	}
	lr, err := i.a.Limit(ctx, id)
	if err != nil {
		i.a.log.Warn("rate limiter", "error", err)
		lr = LimitResult{Allowed: true}
	}
	if !lr.Allowed {
		return ctx, lr, apierr.ResourceExhausted("requests", fmt.Sprintf("limit of %d requests per minute exceeded", lr.Limit))
	}
	return WithIdentity(ctx, id), lr, nil
}

// WrapUnary implements connect.Interceptor.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, lr, err := i.authenticate(ctx, req.Spec().Procedure, req.Header(), req.Peer().Addr)
		if err != nil {
			return nil, err
		}
		res, err := next(ctx, req)
		if res != nil {
			setLimitHeaders(res.Header(), lr)
		}
		return res, err
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, lr, err := i.authenticate(ctx, conn.Spec().Procedure, conn.RequestHeader(), conn.Peer().Addr)
		if err != nil {
			return err
		}
		setLimitHeaders(conn.ResponseHeader(), lr)
		return next(ctx, conn)
	}
}
