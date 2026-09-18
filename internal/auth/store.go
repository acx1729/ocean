package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
)

// Store persists challenges, principals, sessions, devices and agent tokens.
// Authentication is a cross-tenant concern, so it runs system transactions.
type Store struct {
	db *db.DB
}

// NewStore wraps the database.
func NewStore(d *db.DB) *Store { return &Store{db: d} }

// DB exposes the underlying database for handlers that need extra reads.
func (s *Store) DB() *db.DB { return s.db }

// --- challenges ------------------------------------------------------------

// CreateChallenge records a single-use nonce for did.
func (s *Store) CreateChallenge(ctx context.Context, nonce, didStr, message string, expires time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO auth_challenges (nonce, did, message, expires_at) VALUES ($1, $2, $3, $4)`, nonce, didStr, message, expires)
		return err
	})
}

// ConsumeChallenge marks the nonce used and returns the stored message.
func (s *Store) ConsumeChallenge(ctx context.Context, nonce, didStr string, now time.Time) (string, error) {
	var message string
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE auth_challenges SET used_at = $3 WHERE nonce = $1 AND did = $2 AND used_at IS NULL AND expires_at > $3 RETURNING message`, nonce, didStr, now).Scan(&message)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrChallengeExpired
	}
	return message, err
}

// PurgeChallenges deletes expired challenges and old failure records.
func (s *Store) PurgeChallenges(ctx context.Context, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM auth_challenges WHERE expires_at < $1 - interval '1 hour'`, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM auth_failures WHERE at < $1 - interval '1 day'`, now)
		return err
	})
}

// --- failures and lockout --------------------------------------------------

const (
	failureWindow  = 10 * time.Minute
	failureLimit   = 10
	lockoutPeriod  = 15 * time.Minute
	challengeTTL   = 5 * time.Minute
	attestationMsg = "kb device attestation\n"
)

// RecordFailure notes a failed verification for did.
func (s *Store) RecordFailure(ctx context.Context, didStr string, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO auth_failures (did, at) VALUES ($1, $2)`, didStr, now)
		return err
	})
}

// LockedOut reports whether did has too many recent failures.
func (s *Store) LockedOut(ctx context.Context, didStr string, now time.Time) (bool, error) {
	var n int
	var last *time.Time
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), max(at) FROM auth_failures WHERE did = $1 AND at > $2`, didStr, now.Add(-failureWindow)).Scan(&n, &last)
	})
	if err != nil {
		return false, err
	}
	return n >= failureLimit && last != nil && last.Add(lockoutPeriod).After(now), nil
}

// --- principals ------------------------------------------------------------

// Principal is a row of the principals table.
type Principal struct {
	ID            string
	Kind          Kind
	OwnerID       string
	DisplayName   string
	SigningPubkey []byte
	DisabledAt    *time.Time
	CreatedAt     time.Time
}

// EnsureUser creates the user principal on first sign-in.
func (s *Store) EnsureUser(ctx context.Context, didStr string) (*Principal, error) {
	var p Principal
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO principals (id, kind) VALUES ($1, 'user') ON CONFLICT (id) DO NOTHING`, didStr); err != nil {
			return err
		}
		return scanPrincipal(tx.QueryRow(ctx, principalSelect+` WHERE id = $1`, didStr), &p)
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

const principalSelect = `SELECT id, kind, COALESCE(owner_id, ''), COALESCE(display_name, ''), signing_pubkey, disabled_at, created_at FROM principals`

func scanPrincipal(row pgx.Row, p *Principal) error {
	var kind string
	if err := row.Scan(&p.ID, &kind, &p.OwnerID, &p.DisplayName, &p.SigningPubkey, &p.DisabledAt, &p.CreatedAt); err != nil {
		return err
	}
	p.Kind = Kind(kind)
	return nil
}

// GetPrincipal loads a principal; db.ErrNoRows when absent.
func (s *Store) GetPrincipal(ctx context.Context, didStr string) (*Principal, error) {
	var p Principal
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return scanPrincipal(tx.QueryRow(ctx, principalSelect+` WHERE id = $1`, didStr), &p)
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Membership is one workspace membership of a principal.
type Membership struct {
	WorkspaceID string
	Slug        string
	Name        string
	Role        string
	JoinedAt    time.Time
}

// Memberships lists the workspaces a principal belongs to.
func (s *Store) Memberships(ctx context.Context, didStr string) ([]Membership, error) {
	var out []Membership
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT w.id, w.slug, w.name, m.role, m.joined_at FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id WHERE m.principal_id = $1 AND w.deleted_at IS NULL ORDER BY m.joined_at`, didStr)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Membership
			if err := rows.Scan(&m.WorkspaceID, &m.Slug, &m.Name, &m.Role, &m.JoinedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// IsMember reports whether did is a member of the workspace and its role.
func (s *Store) IsMember(ctx context.Context, workspaceID, didStr string) (string, bool, error) {
	var role string
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id = $1 AND principal_id = $2`, workspaceID, didStr).Scan(&role)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return role, err == nil, err
}

// --- sessions --------------------------------------------------------------

// Session is a row of the sessions table.
type Session struct {
	ID          string
	PrincipalID string
	DeviceID    string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// CreateSession stores a session; when the principal already has maxSessions
// active sessions the oldest is revoked.
func (s *Store) CreateSession(ctx context.Context, principal, device string, refreshHash []byte, expires time.Time, maxSessions int, now time.Time) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	err = s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if maxSessions > 0 {
			_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE id IN (
				SELECT id FROM sessions WHERE principal_id = $1 AND revoked_at IS NULL AND expires_at > $3
				ORDER BY created_at DESC OFFSET $2)`, principal, maxSessions-1, now)
			if err != nil {
				return err
			}
		}
		var dev *string
		if device != "" {
			dev = &device
		}
		_, err := tx.Exec(ctx, `INSERT INTO sessions (id, principal_id, device_id, refresh_hash, expires_at, last_seen_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $6)`,
			id, principal, dev, refreshHash, expires, now)
		return err
	})
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// RotateRefresh swaps the refresh hash. Presenting an already rotated token is
// a reuse: the whole session is revoked (committed) and ErrRefreshReused returned.
func (s *Store) RotateRefresh(ctx context.Context, oldHash, newHash []byte, now time.Time) (*Session, error) {
	var sess Session
	var dev *string
	var reused, missing bool
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE sessions SET refresh_hash = $2, prev_refresh_hash = $1, last_seen_at = $3
			WHERE refresh_hash = $1 AND revoked_at IS NULL AND expires_at > $3
			RETURNING id, principal_id, device_id, expires_at, created_at`, oldHash, newHash, now).
			Scan(&sess.ID, &sess.PrincipalID, &dev, &sess.ExpiresAt, &sess.CreatedAt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE prev_refresh_hash = $1 AND revoked_at IS NULL`, oldHash, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			reused = true
			return nil
		}
		missing = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if reused {
		return nil, ErrRefreshReused
	}
	if missing {
		return nil, ErrUnauthenticated
	}
	if dev != nil {
		sess.DeviceID = *dev
	}
	return &sess, nil
}

// RevokeSessionByRefresh revokes the session owning a refresh token.
func (s *Store) RevokeSessionByRefresh(ctx context.Context, hash []byte, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE (refresh_hash = $1 OR prev_refresh_hash = $1) AND revoked_at IS NULL`, hash, now)
		return err
	})
}

// RevokeSession revokes one session by id.
func (s *Store) RevokeSession(ctx context.Context, id string, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, now)
		return err
	})
}

// RevokeAllSessions revokes every session of a principal.
func (s *Store) RevokeAllSessions(ctx context.Context, principal string, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE principal_id = $1 AND revoked_at IS NULL`, principal, now)
		return err
	})
}

// SessionRevoked reports whether a session id is revoked or expired (used by
// long-lived connections that must react before the access token expires).
func (s *Store) SessionRevoked(ctx context.Context, id string, now time.Time) (bool, error) {
	var revoked bool
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revoked_at IS NOT NULL OR expires_at <= $2 FROM sessions WHERE id = $1`, id, now).Scan(&revoked)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return revoked, err
}

// --- devices ---------------------------------------------------------------

// Device is a device principal owned by a user.
type Device struct {
	DID         string
	DisplayName string
	CreatedAt   time.Time
	LastSeenAt  *time.Time
	RevokedAt   *time.Time
}

// RegisterDevice upserts a device principal with its attestation.
func (s *Store) RegisterDevice(ctx context.Context, owner, deviceDID, name string, pubkey, attestation []byte) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO principals (id, kind, owner_id, display_name, signing_pubkey, attestation)
			VALUES ($1, 'device', $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET display_name = EXCLUDED.display_name, attestation = EXCLUDED.attestation, disabled_at = NULL
			WHERE principals.owner_id = EXCLUDED.owner_id`, deviceDID, owner, name, pubkey, attestation)
		return err
	})
}

// ListDevices lists the devices of an owner with their last session activity.
func (s *Store) ListDevices(ctx context.Context, owner string) ([]Device, error) {
	var out []Device
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.id, COALESCE(p.display_name, ''), p.created_at, p.disabled_at,
			(SELECT max(last_seen_at) FROM sessions s WHERE s.device_id = p.id)
			FROM principals p WHERE p.kind = 'device' AND p.owner_id = $1 ORDER BY p.created_at`, owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Device
			if err := rows.Scan(&d.DID, &d.DisplayName, &d.CreatedAt, &d.RevokedAt, &d.LastSeenAt); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeDevice disables the device principal and revokes its sessions.
func (s *Store) RevokeDevice(ctx context.Context, owner, deviceDID string, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE principals SET disabled_at = $3 WHERE id = $1 AND owner_id = $2 AND kind = 'device'`, deviceDID, owner, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return db.ErrNoRows
		}
		_, err = tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE device_id = $1 AND revoked_at IS NULL`, deviceDID, now)
		return err
	})
}

// --- agent tokens ----------------------------------------------------------

// AgentToken is a row of agent_tokens joined with its principal.
type AgentToken struct {
	WorkspaceID    string
	ID             string
	PrincipalID    string
	OwnerID        string
	Name           string
	Scopes         []Scope
	Tools          []string
	ExpiresAt      *time.Time
	LastUsedAt     *time.Time
	RevokedAt      *time.Time
	ThrottledUntil *time.Time
	CreatedAt      time.Time
}

const agentSelect = `SELECT workspace_id, id, principal_id, owner_id, name, scopes, tools, expires_at, last_used_at, revoked_at, throttled_until, created_at FROM agent_tokens`

func scanAgent(row pgx.Row, a *AgentToken) error {
	var scopes []byte
	if err := row.Scan(&a.WorkspaceID, &a.ID, &a.PrincipalID, &a.OwnerID, &a.Name, &scopes, &a.Tools, &a.ExpiresAt, &a.LastUsedAt, &a.RevokedAt, &a.ThrottledUntil, &a.CreatedAt); err != nil {
		return err
	}
	return json.Unmarshal(scopes, &a.Scopes)
}

// CreateAgentToken stores the token hash, its agent principal and scopes.
func (s *Store) CreateAgentToken(ctx context.Context, a *AgentToken, hash []byte) error {
	scopes, err := json.Marshal(a.Scopes)
	if err != nil {
		return err
	}
	if a.Tools == nil {
		a.Tools = []string{}
	}
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO principals (id, kind, owner_id, display_name) VALUES ($1, 'agent', $2, $3)`, a.PrincipalID, a.OwnerID, a.Name); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO agent_tokens (workspace_id, principal_id, owner_id, name, token_hash, scopes, tools, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at`,
			a.WorkspaceID, a.PrincipalID, a.OwnerID, a.Name, hash, scopes, a.Tools, a.ExpiresAt).Scan(&a.ID, &a.CreatedAt)
	})
}

// AgentTokenByHash resolves a presented token.
func (s *Store) AgentTokenByHash(ctx context.Context, hash []byte) (*AgentToken, error) {
	var a AgentToken
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return scanAgent(tx.QueryRow(ctx, agentSelect+` WHERE token_hash = $1`, hash), &a)
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAgentToken loads one token of a workspace.
func (s *Store) GetAgentToken(ctx context.Context, workspaceID, id string) (*AgentToken, error) {
	var a AgentToken
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return scanAgent(tx.QueryRow(ctx, agentSelect+` WHERE workspace_id = $1 AND id = $2`, workspaceID, id), &a)
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAgentTokens lists tokens of a workspace, optionally for one owner, in
// creation order with a keyset cursor on (created_at, id).
func (s *Store) ListAgentTokens(ctx context.Context, workspaceID, owner string, afterCreated time.Time, afterID string, limit int) ([]AgentToken, error) {
	var out []AgentToken
	err := s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, agentSelect+` WHERE workspace_id = $1 AND ($2 = '' OR owner_id = $2)
			AND (created_at, id) > ($3, $4::uuid) ORDER BY created_at, id LIMIT $5`,
			workspaceID, owner, afterCreated, nullUUID(afterID), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a AgentToken
			if err := scanAgent(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

func nullUUID(s string) string {
	if s == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return s
}

// RevokeAgentToken revokes a token and disables its principal.
func (s *Store) RevokeAgentToken(ctx context.Context, workspaceID, id string, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var principal string
		err := tx.QueryRow(ctx, `UPDATE agent_tokens SET revoked_at = $3 WHERE workspace_id = $1 AND id = $2 AND revoked_at IS NULL RETURNING principal_id`, workspaceID, id, now).Scan(&principal)
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ErrNoRows
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE principals SET disabled_at = $2 WHERE id = $1`, principal, now)
		return err
	})
}

// TouchAgentToken records use.
func (s *Store) TouchAgentToken(ctx context.Context, workspaceID, id string, now time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_tokens SET last_used_at = $3 WHERE workspace_id = $1 AND id = $2`, workspaceID, id, now)
		return err
	})
}

// ThrottleAgentToken disables a token until the given time after a burst of failures.
func (s *Store) ThrottleAgentToken(ctx context.Context, workspaceID, id string, until time.Time) error {
	return s.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_tokens SET throttled_until = $3 WHERE workspace_id = $1 AND id = $2`, workspaceID, id, until)
		return err
	})
}

// String implements fmt.Stringer without leaking anything sensitive.
func (a *AgentToken) String() string {
	return fmt.Sprintf("agent token %s (%s) owned by %s", a.ID, a.Name, a.OwnerID)
}
