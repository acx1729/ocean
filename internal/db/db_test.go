package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDSN points at a database this test owns. The default matches the local
// cluster used during development; CI sets KB_TEST_DATABASE_URL.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KB_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@127.0.0.1:55432/kb_db_test?sslmode=disable"
	}
	return dsn
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)
	admin, err := pgx.Connect(ctx, "postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable")
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS kb_db_test")
	if _, err := admin.Exec(ctx, "CREATE DATABASE kb_db_test"); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	d, err := Open(ctx, dsn, "", Options{MaxConns: 4, AppName: "kb-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func TestMigrateIsIdempotentAndRLSIsolates(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	v1, err := d.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := d.Migrate(ctx)
	if err != nil || v2 != v1 || v1 < 1 {
		t.Fatalf("second migrate: v1=%d v2=%d err=%v", v1, v2, err)
	}
	want, _ := SchemaVersion()
	if want != v1 {
		t.Fatalf("SchemaVersion()=%d, db=%d", want, v1)
	}

	var wsA, wsB string
	err = d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, ws := range []struct {
			slug string
			dst  *string
		}{{"alpha", &wsA}, {"beta", &wsB}} {
			if err := tx.QueryRow(ctx, `INSERT INTO workspaces (slug, name, fga_store_id, fga_model_id) VALUES ($1, $1, 's', 'm') RETURNING id`, ws.slug).Scan(ws.dst); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO projects (workspace_id, slug, name) VALUES ($1, 'main', 'Main')`, *ws.dst); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO principals (id, kind) VALUES ('did:key:zAlice', 'user')`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, principal_id, role) VALUES ($1, 'did:key:zAlice', 'admin')`, wsA)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	count := func(tn *Tenant, sql string, args ...any) int {
		var n int
		err := d.Tx(ctx, tn, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(&n)
		})
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(nil, `SELECT count(*) FROM projects`); n != 2 {
		t.Fatalf("system sees %d projects, want 2", n)
	}
	if n := count(&Tenant{WorkspaceID: wsA, PrincipalID: "did:key:zAlice"}, `SELECT count(*) FROM projects`); n != 1 {
		t.Fatalf("tenant A sees %d projects, want 1", n)
	}
	// A predicate naming the other workspace still returns nothing under RLS.
	if n := count(&Tenant{WorkspaceID: wsA}, `SELECT count(*) FROM projects WHERE workspace_id = $1`, wsB); n != 0 {
		t.Fatalf("tenant A sees %d of B's projects, want 0", n)
	}
	if n := count(&Tenant{WorkspaceID: ""}, `SELECT count(*) FROM projects`); n != 0 {
		t.Fatalf("no tenant sees %d projects, want 0", n)
	}
	// A member sees the workspaces they belong to without a tenant set.
	if n := count(&Tenant{PrincipalID: "did:key:zAlice"}, `SELECT count(*) FROM workspaces`); n != 1 {
		t.Fatalf("alice sees %d workspaces, want 1", n)
	}
	// Writes into another workspace are refused by the policy.
	err = d.Tx(ctx, &Tenant{WorkspaceID: wsA}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects (workspace_id, slug, name) VALUES ($1, 'evil', 'Evil')`, wsB)
		return err
	})
	if !IsRLSViolation(err) {
		t.Fatalf("cross-tenant insert should violate RLS, got %v", err)
	}
	// Statement timeout applies to tenant transactions.
	err = d.TxWith(ctx, &Tenant{WorkspaceID: wsA}, TxOptions{StatementTimeout: 50 * time.Millisecond}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT pg_sleep(1)`)
		return err
	})
	if !IsStatementTimeout(err) {
		t.Fatalf("expected statement timeout, got %v", err)
	}
}
