package app

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/keyring"
)

// keyStore adapts the workspace_keys table to keyring.Store.
type keyStore struct {
	db      *db.DB
	nodeDID string
}

var _ keyring.Store = (*keyStore)(nil)

func (k *keyStore) WrappedKey(ctx context.Context, workspaceID string, version int) ([]byte, error) {
	var wrapped []byte
	err := k.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT wrapped_key FROM workspace_keys WHERE workspace_id = $1 AND key_version = $2 AND recipient_id = $3`, workspaceID, version, k.nodeDID).Scan(&wrapped)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, keyring.ErrNoKey
	}
	return wrapped, err
}

func (k *keyStore) CurrentVersion(ctx context.Context, workspaceID string) (int, error) {
	var v int
	err := k.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_key_version FROM workspaces WHERE id = $1`, workspaceID).Scan(&v)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, keyring.ErrNoKey
	}
	return v, err
}
