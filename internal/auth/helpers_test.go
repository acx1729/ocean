package auth

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// createWorkspace inserts a workspace with the given member as admin directly
// (WorkspacesService lives in another package).
func createWorkspace(t *testing.T, h *harness, member string) string {
	t.Helper()
	var ws string
	err := h.store.db.System(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO workspaces (slug, name, fga_store_id, fga_model_id) VALUES ('test-ws', 'Test', 's', 'm') RETURNING id`).Scan(&ws); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, principal_id, role) VALUES ($1, $2, 'admin')`, ws, member)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
