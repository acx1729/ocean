// Package testutil provides helpers for integration tests against the local
// PostgreSQL cluster. Tests skip when the cluster is unreachable.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
)

// AdminDSN is the maintenance connection used to create test databases.
func AdminDSN() string {
	if v := os.Getenv("KB_TEST_ADMIN_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable"
}

// NewDB creates a fresh migrated database for the test and drops it afterwards.
func NewDB(t testing.TB) *db.DB {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, AdminDSN())
	if err != nil {
		if os.Getenv("KB_TEST_REQUIRE_DB") != "" {
			t.Fatalf("postgres not reachable at %s (KB_TEST_REQUIRE_DB is set): %v", AdminDSN(), err)
		}
		t.Skipf("postgres not reachable at %s: %v", AdminDSN(), err)
	}
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	name := fmt.Sprintf("kbtest_%s_%s", sanitize(t.Name()), hex.EncodeToString(suffix[:]))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	admin.Close(ctx)
	dsn := strings.Replace(AdminDSN(), "/postgres?", "/"+name+"?", 1)
	d, err := db.Open(ctx, dsn, "", db.Options{MaxConns: 8, AppName: "kb-test"})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if _, err := d.Migrate(ctx); err != nil {
		d.Close()
		t.Fatalf("migrate test database: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		admin, err := pgx.Connect(context.Background(), AdminDSN())
		if err != nil {
			return
		}
		defer admin.Close(context.Background())
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return d
}

func sanitize(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 24 {
			break
		}
	}
	return b.String()
}
