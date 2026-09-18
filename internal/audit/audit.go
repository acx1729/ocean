// Package audit appends to the per-workspace SHA-256 hash chain in audit_log
// (specification section 7, "Audit") and verifies it.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Entry is one audited action.
type Entry struct {
	Principal    string
	Owner        string
	Action       string
	ResourceType string
	ResourceID   string
	Detail       map[string]any
}

// canonical is the hashed representation; field order is fixed by the struct.
type canonical struct {
	At           string         `json:"at"`
	Principal    string         `json:"principal"`
	Owner        string         `json:"owner,omitempty"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id"`
	Detail       map[string]any `json:"detail"`
}

// genesis is the prev_hash of the first entry of a workspace.
var genesis = sha256.Sum256([]byte("kb-audit-genesis"))

func hashRow(prev []byte, c canonical) ([]byte, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(body)
	return h.Sum(nil), nil
}

// Append writes an entry linked to the previous one. It takes a per-workspace
// advisory lock so concurrent writers cannot fork the chain.
func Append(ctx context.Context, tx pgx.Tx, workspaceID string, e Entry, now time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, workspaceID+":audit"); err != nil {
		return err
	}
	prev := genesis[:]
	var last []byte
	err := tx.QueryRow(ctx, `SELECT hash FROM audit_log WHERE workspace_id = $1 ORDER BY id DESC LIMIT 1`, workspaceID).Scan(&last)
	switch {
	case err == nil:
		prev = last
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return err
	}
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	at := now.UTC().Truncate(time.Microsecond)
	c := canonical{At: at.Format(time.RFC3339Nano), Principal: e.Principal, Owner: e.Owner, Action: e.Action, ResourceType: e.ResourceType, ResourceID: e.ResourceID, Detail: e.Detail}
	h, err := hashRow(prev, c)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(e.Detail)
	var owner *string
	if e.Owner != "" && e.Owner != e.Principal {
		owner = &e.Owner
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (workspace_id, at, principal_id, owner_id, action, resource_type, resource_id, detail, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		workspaceID, at, e.Principal, owner, e.Action, e.ResourceType, e.ResourceID, detail, prev, h)
	return err
}

// Report is the outcome of a verification walk.
type Report struct {
	OK       bool
	Entries  int64
	BrokenAt int64 // id of the first entry whose hash does not match, 0 when OK
}

// Verify walks the chain of a workspace in id order and recomputes every hash.
func Verify(ctx context.Context, tx pgx.Tx, workspaceID string) (*Report, error) {
	rows, err := tx.Query(ctx, `SELECT id, at, principal_id, COALESCE(owner_id, ''), action, resource_type, resource_id, detail, prev_hash, hash
		FROM audit_log WHERE workspace_id = $1 ORDER BY id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rep := &Report{OK: true}
	prev := genesis[:]
	for rows.Next() {
		var id int64
		var at time.Time
		var c canonical
		var detail, prevHash, hash []byte
		if err := rows.Scan(&id, &at, &c.Principal, &c.Owner, &c.Action, &c.ResourceType, &c.ResourceID, &detail, &prevHash, &hash); err != nil {
			return nil, err
		}
		rep.Entries++
		c.At = at.UTC().Format(time.RFC3339Nano)
		if err := json.Unmarshal(detail, &c.Detail); err != nil {
			return nil, fmt.Errorf("audit entry %d: %w", id, err)
		}
		want, err := hashRow(prev, c)
		if err != nil {
			return nil, err
		}
		if string(prevHash) != string(prev) || string(want) != string(hash) {
			rep.OK = false
			rep.BrokenAt = id
			return rep, rows.Err()
		}
		prev = hash
	}
	return rep, rows.Err()
}
