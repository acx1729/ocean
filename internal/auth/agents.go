package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/did"
)

// KnownTools are the MCP tools an agent token may be allowed to call.
var KnownTools = []string{
	"search_blocks", "get_page", "get_block", "get_backlinks", "list_projects", "list_types",
	"create_page", "append_blocks", "update_block", "set_type", "publish_page",
}

// ScopeValidator confirms that the owner may delegate each scope (an FGA check
// as the owner). It is provided by the permissions layer.
type ScopeValidator interface {
	ValidateScopes(ctx context.Context, owner, workspaceID string, scopes []Scope) error
}

// AgentsService implements kb.v1.AgentsService.
type AgentsService struct {
	store  *Store
	auth   *Authenticator
	scopes ScopeValidator
	clock  Clock
}

var _ kbv1connect.AgentsServiceHandler = (*AgentsService)(nil)

// NewAgentsService builds the service. scopes may be nil until the permission
// layer is wired; membership in the workspace is always required.
func NewAgentsService(store *Store, a *Authenticator, scopes ScopeValidator, clock Clock) *AgentsService {
	if clock == nil {
		clock = time.Now
	}
	return &AgentsService{store: store, auth: a, scopes: scopes, clock: clock}
}

// MintAgentDID returns a fresh did:key for an agent principal; the private key
// is discarded because agents authenticate with bearer tokens, not signatures.
func MintAgentDID() (string, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return did.FromEd25519(pub), nil
}

func scopesFromProto(in []*kbv1.Scope) ([]Scope, error) {
	out := make([]Scope, 0, len(in))
	for i, s := range in {
		var rt string
		switch s.GetResourceType() {
		case kbv1.ResourceType_RESOURCE_TYPE_WORKSPACE:
			rt = "workspace"
		case kbv1.ResourceType_RESOURCE_TYPE_PROJECT:
			rt = "project"
		case kbv1.ResourceType_RESOURCE_TYPE_DOC:
			rt = "doc"
		default:
			return nil, apierr.InvalidArgument(fmt.Sprintf("scopes[%d].resource_type", i), "must be workspace, project or doc")
		}
		if s.GetResourceId() == "" || s.GetRole() == "" {
			return nil, apierr.InvalidArgument(fmt.Sprintf("scopes[%d]", i), "resource_id and role are required")
		}
		out = append(out, Scope{ResourceType: rt, ResourceID: s.GetResourceId(), Role: s.GetRole()})
	}
	return out, nil
}

func scopesToProto(in []Scope) []*kbv1.Scope {
	out := make([]*kbv1.Scope, 0, len(in))
	for _, s := range in {
		rt := kbv1.ResourceType_RESOURCE_TYPE_UNSPECIFIED
		switch s.ResourceType {
		case "workspace":
			rt = kbv1.ResourceType_RESOURCE_TYPE_WORKSPACE
		case "project":
			rt = kbv1.ResourceType_RESOURCE_TYPE_PROJECT
		case "doc":
			rt = kbv1.ResourceType_RESOURCE_TYPE_DOC
		}
		out = append(out, &kbv1.Scope{ResourceType: rt, ResourceId: s.ResourceID, Role: s.Role})
	}
	return out
}

func toAgentToken(a *AgentToken) *kbv1.AgentToken {
	return &kbv1.AgentToken{
		Id: a.ID, PrincipalDid: a.PrincipalID, OwnerDid: a.OwnerID, Name: a.Name, Scopes: scopesToProto(a.Scopes), Tools: a.Tools,
		ExpiresAt: tsOrNil(a.ExpiresAt), LastUsedAt: tsOrNil(a.LastUsedAt), RevokedAt: tsOrNil(a.RevokedAt),
		CreatedAt: timestamppb.New(a.CreatedAt), WorkspaceId: a.WorkspaceID,
	}
}

// CreateToken mints a scoped agent token owned by the caller.
func (s *AgentsService) CreateToken(ctx context.Context, req *connect.Request[kbv1.CreateTokenRequest]) (*connect.Response[kbv1.CreateTokenResponse], error) {
	id, err := MustIdentity(ctx)
	if err != nil {
		return nil, apierr.Unauthenticated("")
	}
	if id.Kind != KindUser {
		return nil, apierr.PermissionDenied("create_agent_token", "agents cannot mint agent tokens", false)
	}
	ws := req.Msg.GetWorkspaceId()
	if ws == "" {
		return nil, apierr.InvalidArgument("workspace_id", "required")
	}
	name := strings.TrimSpace(req.Msg.GetName())
	if name == "" || len(name) > 120 {
		return nil, apierr.InvalidArgument("name", "1 to 120 characters")
	}
	if _, member, err := s.store.IsMember(ctx, ws, id.Principal); err != nil {
		return nil, apierr.Internal(err)
	} else if !member {
		return nil, apierr.NotFound("workspace", ws)
	}
	scopes, err := scopesFromProto(req.Msg.GetScopes())
	if err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		return nil, apierr.InvalidArgument("scopes", "at least one scope is required")
	}
	for i, t := range req.Msg.GetTools() {
		known := false
		for _, k := range KnownTools {
			if t == k {
				known = true
				break
			}
		}
		if !known {
			return nil, apierr.InvalidArgument(fmt.Sprintf("tools[%d]", i), "unknown tool "+t)
		}
	}
	if s.scopes != nil {
		if err := s.scopes.ValidateScopes(ctx, id.Principal, ws, scopes); err != nil {
			var ce *connect.Error
			if errors.As(err, &ce) {
				return nil, err
			}
			return nil, apierr.PermissionDenied("delegate", ws, true)
		}
	}
	var expires *time.Time
	if req.Msg.GetExpiresAt() != nil {
		t := req.Msg.GetExpiresAt().AsTime()
		if !t.After(s.clock()) {
			return nil, apierr.InvalidArgument("expires_at", "must be in the future")
		}
		expires = &t
	}
	agentDID, err := MintAgentDID()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	token, err := NewOpaqueToken(AgentPrefix)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	row := &AgentToken{WorkspaceID: ws, PrincipalID: agentDID, OwnerID: id.Principal, Name: name, Scopes: scopes, Tools: req.Msg.GetTools(), ExpiresAt: expires}
	if err := s.store.CreateAgentToken(ctx, row, HashToken(token)); err != nil {
		return nil, apierr.Internal(err)
	}
	return connect.NewResponse(&kbv1.CreateTokenResponse{Token: token, Agent: toAgentToken(row)}), nil
}

func decodeCursor(c string) (time.Time, string, error) {
	if c == "" {
		return time.Time{}, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return time.Time{}, "", errors.New("bad cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return t, id, err
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// ListTokens lists the caller's tokens in a workspace (all tokens for admins).
func (s *AgentsService) ListTokens(ctx context.Context, req *connect.Request[kbv1.ListTokensRequest]) (*connect.Response[kbv1.ListTokensResponse], error) {
	id, err := MustIdentity(ctx)
	if err != nil {
		return nil, apierr.Unauthenticated("")
	}
	ws := req.Msg.GetWorkspaceId()
	role, member, err := s.store.IsMember(ctx, ws, id.EffectiveOwner())
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if !member {
		return nil, apierr.NotFound("workspace", ws)
	}
	owner := id.EffectiveOwner()
	if role == "admin" {
		owner = ""
	}
	size := int(req.Msg.GetPageSize())
	if size <= 0 {
		size = 50
	}
	if size > 500 {
		size = 500
	}
	afterT, afterID, err := decodeCursor(req.Msg.GetCursor())
	if err != nil {
		return nil, apierr.InvalidArgument("cursor", "invalid")
	}
	rows, err := s.store.ListAgentTokens(ctx, ws, owner, afterT, afterID, size+1)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := &kbv1.ListTokensResponse{}
	for i, a := range rows {
		if i == size {
			out.NextCursor = encodeCursor(rows[i-1].CreatedAt, rows[i-1].ID)
			break
		}
		out.Tokens = append(out.Tokens, toAgentToken(&rows[i]))
		_ = a
	}
	return connect.NewResponse(out), nil
}

func (s *AgentsService) load(ctx context.Context, ws, id string) (*AgentToken, *Identity, error) {
	caller, err := MustIdentity(ctx)
	if err != nil {
		return nil, nil, apierr.Unauthenticated("")
	}
	tok, err := s.store.GetAgentToken(ctx, ws, id)
	if err != nil {
		if errors.Is(err, db.ErrNoRows) {
			return nil, nil, apierr.NotFound("agent token", id)
		}
		return nil, nil, apierr.Internal(err)
	}
	role, member, err := s.store.IsMember(ctx, ws, caller.EffectiveOwner())
	if err != nil {
		return nil, nil, apierr.Internal(err)
	}
	if !member || (tok.OwnerID != caller.EffectiveOwner() && role != "admin") {
		return nil, nil, apierr.NotFound("agent token", id)
	}
	return tok, caller, nil
}

// RevokeToken revokes a token immediately.
func (s *AgentsService) RevokeToken(ctx context.Context, req *connect.Request[kbv1.RevokeTokenRequest]) (*connect.Response[kbv1.RevokeTokenResponse], error) {
	tok, _, err := s.load(ctx, req.Msg.GetWorkspaceId(), req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeAgentToken(ctx, tok.WorkspaceID, tok.ID, s.clock()); err != nil && !errors.Is(err, db.ErrNoRows) {
		return nil, apierr.Internal(err)
	}
	if s.auth != nil {
		// The cache is keyed by token hash, which we no longer have; entries expire within 30 s and the revoked flag is re-read then.
		s.auth.mu.Lock()
		for k, e := range s.auth.cache {
			if e.tok.ID == tok.ID {
				delete(s.auth.cache, k)
			}
		}
		s.auth.mu.Unlock()
	}
	return connect.NewResponse(&kbv1.RevokeTokenResponse{}), nil
}

// GetToken returns one token's metadata.
func (s *AgentsService) GetToken(ctx context.Context, req *connect.Request[kbv1.GetTokenRequest]) (*connect.Response[kbv1.GetTokenResponse], error) {
	tok, _, err := s.load(ctx, req.Msg.GetWorkspaceId(), req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.GetTokenResponse{Agent: toAgentToken(tok)}), nil
}
