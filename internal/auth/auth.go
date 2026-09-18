// Package auth implements identity and authentication (specification section 4):
// principals are public keys expressed as DIDs, users prove control of a key
// through a signed challenge (SIWE for wallets, a signed message for did:key),
// the node issues short-lived PASETO access tokens and rotating refresh tokens,
// and every other principal (device, agent token, share link) is owned by a user.
package auth

import (
	"context"
	"errors"
	"time"
)

// Kind is the principal kind of an authenticated caller.
type Kind string

const (
	KindUser     Kind = "user"
	KindDevice   Kind = "device"
	KindAgent    Kind = "agent"
	KindLink     Kind = "link"
	KindNode     Kind = "node"
	KindOperator Kind = "operator"
)

// Scope is one resource grant carried by an agent token.
type Scope struct {
	ResourceType string `json:"resource_type"` // workspace, project, doc
	ResourceID   string `json:"resource_id"`
	Role         string `json:"role"`
}

// Identity is the resolved caller of a request.
type Identity struct {
	// Principal is the acting DID (the agent for agent tokens).
	Principal string
	Kind      Kind
	// Owner is the user behind a device, agent or link; equals Principal for users.
	Owner     string
	SessionID string
	DeviceID  string
	// Agent token details, when Kind == KindAgent.
	AgentTokenID string
	WorkspaceID  string
	Scopes       []Scope
	Tools        []string
	// Operator marks the node-operator token used for AdminService.
	Operator bool
	// Anonymous marks a share-link viewer session.
	Anonymous bool
	ExpiresAt time.Time
}

// IsAgent reports whether checks must run twice (as the agent and as the owner).
func (id *Identity) IsAgent() bool { return id != nil && id.Kind == KindAgent }

// EffectiveOwner returns the user whose permissions bound the caller.
func (id *Identity) EffectiveOwner() string {
	if id.Owner != "" {
		return id.Owner
	}
	return id.Principal
}

type ctxKey int

const identityKey ctxKey = iota

// WithIdentity attaches an identity to ctx.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// FromContext returns the caller identity, if authenticated.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey).(*Identity)
	return id, ok && id != nil
}

// MustIdentity returns the identity or ErrUnauthenticated.
func MustIdentity(ctx context.Context) (*Identity, error) {
	id, ok := FromContext(ctx)
	if !ok {
		return nil, ErrUnauthenticated
	}
	return id, nil
}

// Errors returned by the package; the service layer maps them to Connect codes.
var (
	ErrUnauthenticated  = errors.New("auth: unauthenticated")
	ErrInvalidSignature = errors.New("auth: invalid signature")
	ErrChallengeExpired = errors.New("auth: challenge expired or already used")
	ErrTokenExpired     = errors.New("auth: token expired")
	ErrSessionRevoked   = errors.New("auth: session revoked")
	ErrRefreshReused    = errors.New("auth: refresh token reuse detected; session revoked")
	ErrDisabled         = errors.New("auth: principal disabled")
	ErrLockedOut        = errors.New("auth: too many failed verifications")
	ErrRateLimited      = errors.New("auth: rate limited")
	ErrContractWallet   = errors.New("auth: contract wallets require KB_EVM_RPC_URL")
	ErrTooManySessions  = errors.New("auth: too many concurrent sessions")
)

// Clock abstracts time for tests.
type Clock func() time.Time
