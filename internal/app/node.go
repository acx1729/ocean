package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/keyring"
)

// loadNodeKey unseals (or creates) the node identity key according to the
// configured custody and records the node in the database.
func (a *App) loadNodeKey(ctx context.Context) error {
	if a.cfg.NodeKeyKMS != "" {
		return errors.New("KB_NODE_KEY_KMS: cloud KMS unwrapping is not implemented in this build; use KB_NODE_KEY_PASSPHRASE with a Secret-mounted sealed file")
	}
	path := a.cfg.NodeKeyPath()
	node, created, err := keyring.LoadOrCreateNodeKey(path, []byte(a.cfg.NodeKeyPassphrase), keyring.DefaultSealParams)
	if err != nil {
		return fmt.Errorf("node key: %w", err)
	}
	a.node = node
	if created {
		a.log.Info("generated node identity key", "path", path, "node_did", node.DID())
	}
	a.operatorToken = a.cfg.OperatorToken
	if a.operatorToken == "" {
		a.operatorToken = auth.OperatorToken(node)
		tokenPath := path + ".operator-token"
		if _, statErr := os.Stat(tokenPath); errors.Is(statErr, os.ErrNotExist) {
			if err := os.WriteFile(tokenPath, []byte(a.operatorToken+"\n"), 0o600); err != nil {
				a.log.Warn("could not write operator token file", "path", tokenPath, "error", err)
			} else {
				a.log.Info("operator token written; it authenticates AdminService", "path", tokenPath)
			}
		}
	}
	return a.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO principals (id, kind, display_name) VALUES ($1, 'node', 'node') ON CONFLICT (id) DO NOTHING`, node.DID()); err != nil {
			return err
		}
		var admin *string
		if a.cfg.BootstrapAdminDID != "" {
			s := a.cfg.BootstrapAdminDID
			admin = &s
		}
		_, err := tx.Exec(ctx, `INSERT INTO node (did, signing_pubkey, encryption_pubkey, bootstrap_admin)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (singleton) DO UPDATE SET did = EXCLUDED.did, signing_pubkey = EXCLUDED.signing_pubkey,
			  encryption_pubkey = EXCLUDED.encryption_pubkey,
			  bootstrap_admin = COALESCE(node.bootstrap_admin, EXCLUDED.bootstrap_admin)`,
			node.DID(), []byte(node.SigningPublicKey()), node.EncryptionPublicKey(), admin)
		return err
	})
}
