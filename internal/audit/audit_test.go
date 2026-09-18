package audit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/testutil"
)

func TestChainAppendAndVerify(t *testing.T) {
	d := testutil.NewDB(t)
	ctx := context.Background()
	var ws string
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO workspaces (slug, name, fga_store_id, fga_model_id) VALUES ('audit', 'A', 's', 'm') RETURNING id`).Scan(&ws); err != nil {
			return err
		}
		now := time.Now()
		for i := 0; i < 5; i++ {
			if err := Append(ctx, tx, ws, Entry{Principal: "did:key:zA", Action: "block.update", ResourceType: "block", ResourceID: "b1", Detail: map[string]any{"i": i, "nested": map[string]any{"k": "v"}}}, now.Add(time.Duration(i)*time.Millisecond)); err != nil {
				return err
			}
		}
		rep, err := Verify(ctx, tx, ws)
		if err != nil || !rep.OK || rep.Entries != 5 {
			t.Fatalf("verify: %v %+v", err, rep)
		}
		// Tampering with a row breaks the chain at that row.
		if _, err := tx.Exec(ctx, `UPDATE audit_log SET action = 'block.delete' WHERE workspace_id = $1 AND id = (SELECT id FROM audit_log WHERE workspace_id = $1 ORDER BY id OFFSET 2 LIMIT 1)`, ws); err != nil {
			return err
		}
		rep, err = Verify(ctx, tx, ws)
		if err != nil || rep.OK || rep.BrokenAt == 0 {
			t.Fatalf("tamper not detected: %v %+v", err, rep)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
