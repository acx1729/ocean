package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/acx1729/ocean/internal/keyring"
)

// Token prefixes make token kinds distinguishable in logs and secret scanners.
const (
	RefreshPrefix  = "kbr_"
	AgentPrefix    = "kba_"
	OperatorPrefix = "kbo_"
	LinkPrefix     = "kbl_"
)

// Claims carried by an access token.
type Claims struct {
	Subject   string    // acting DID
	Owner     string    // owning user DID (equals Subject for users)
	Kind      Kind      // user, device, link
	SessionID string    // sid
	DeviceID  string    // dev
	IssuedAt  time.Time // iat
	ExpiresAt time.Time // exp
	Anonymous bool
	// WorkspaceID scopes anonymous link sessions.
	WorkspaceID string
}

// TokenIssuer signs and verifies PASETO v4.local access tokens with a key
// derived from the node key, so every replica of a node verifies locally.
type TokenIssuer struct {
	key      paseto.V4SymmetricKey
	issuer   string
	ttl      time.Duration
	parser   paseto.Parser
	implicit []byte
}

// NewTokenIssuer derives the token key from node and binds tokens to issuer
// (the public URL), which prevents a token minted by one node from being
// accepted by another node that happens to share nothing but a schema.
func NewTokenIssuer(node *keyring.NodeKey, issuer string, ttl time.Duration) (*TokenIssuer, error) {
	raw := node.Derive("paseto-v4-local-access", 32)
	key, err := paseto.V4SymmetricKeyFromBytes(raw)
	keyring.Zero(raw)
	if err != nil {
		return nil, err
	}
	p := paseto.NewParser()
	p.AddRule(paseto.NotExpired())
	p.AddRule(paseto.ValidAt(time.Now()))
	p.AddRule(paseto.IssuedBy(issuer))
	return &TokenIssuer{key: key, issuer: issuer, ttl: ttl, parser: p, implicit: []byte("kb-access-v1")}, nil
}

// TTL is the access token lifetime.
func (t *TokenIssuer) TTL() time.Duration { return t.ttl }

// Issue mints an access token for c, filling IssuedAt and ExpiresAt.
func (t *TokenIssuer) Issue(c Claims, now time.Time) (string, Claims, error) {
	c.IssuedAt = now
	c.ExpiresAt = now.Add(t.ttl)
	tok := paseto.NewToken()
	tok.SetIssuer(t.issuer)
	tok.SetSubject(c.Subject)
	tok.SetIssuedAt(c.IssuedAt)
	tok.SetNotBefore(c.IssuedAt.Add(-30 * time.Second))
	tok.SetExpiration(c.ExpiresAt)
	if err := tok.Set("sid", c.SessionID); err != nil {
		return "", c, err
	}
	if err := tok.Set("dev", c.DeviceID); err != nil {
		return "", c, err
	}
	if err := tok.Set("own", c.Owner); err != nil {
		return "", c, err
	}
	if err := tok.Set("knd", string(c.Kind)); err != nil {
		return "", c, err
	}
	if c.Anonymous {
		if err := tok.Set("anon", true); err != nil {
			return "", c, err
		}
		if err := tok.Set("ws", c.WorkspaceID); err != nil {
			return "", c, err
		}
	}
	return tok.V4Encrypt(t.key, t.implicit), c, nil
}

// Verify decrypts and validates an access token.
func (t *TokenIssuer) Verify(token string, now time.Time) (Claims, error) {
	p := paseto.NewParser()
	p.AddRule(paseto.ValidAt(now))
	p.AddRule(paseto.IssuedBy(t.issuer))
	tok, err := p.ParseV4Local(t.key, token, t.implicit)
	if err != nil {
		if strings.Contains(err.Error(), "expired") {
			return Claims{}, ErrTokenExpired
		}
		return Claims{}, ErrUnauthenticated
	}
	var c Claims
	c.Subject, _ = tok.GetSubject()
	c.IssuedAt, _ = tok.GetIssuedAt()
	c.ExpiresAt, _ = tok.GetExpiration()
	_ = tok.Get("sid", &c.SessionID)
	_ = tok.Get("dev", &c.DeviceID)
	_ = tok.Get("own", &c.Owner)
	var kind string
	_ = tok.Get("knd", &kind)
	c.Kind = Kind(kind)
	var anon bool
	_ = tok.Get("anon", &anon)
	c.Anonymous = anon
	_ = tok.Get("ws", &c.WorkspaceID)
	if c.Subject == "" || c.Kind == "" {
		return Claims{}, ErrUnauthenticated
	}
	if c.Owner == "" {
		c.Owner = c.Subject
	}
	return c, nil
}

// NewOpaqueToken returns prefix + 43 base64url characters of 256 random bits.
func NewOpaqueToken(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken returns the SHA-256 of a token; only hashes are stored.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// OperatorToken derives the node-operator token from the node key. It is
// written next to the sealed key file at first start and authenticates
// AdminService independently of workspace auth.
func OperatorToken(node *keyring.NodeKey) string {
	return OperatorPrefix + base64.RawURLEncoding.EncodeToString(node.Derive("operator-token", 32))
}

// ConstantTimeEqual compares two strings without leaking their difference.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ParseBearer extracts the bearer token from an Authorization header.
func ParseBearer(header string) (string, error) {
	const p = "bearer "
	if len(header) > len(p) && strings.EqualFold(header[:len(p)], p) {
		tok := strings.TrimSpace(header[len(p):])
		if tok != "" {
			return tok, nil
		}
	}
	return "", errors.New("auth: missing bearer token")
}

// TokenKind classifies a presented bearer token by prefix.
func TokenKind(token string) Kind {
	switch {
	case strings.HasPrefix(token, AgentPrefix):
		return KindAgent
	case strings.HasPrefix(token, OperatorPrefix):
		return KindOperator
	case strings.HasPrefix(token, "v4.local."):
		return KindUser
	}
	return ""
}

// Redact shortens a token for error messages.
func Redact(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return fmt.Sprintf("%s…(%d)", token[:8], len(token))
}
