package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func minimal() map[string]string {
	return map[string]string{
		"KB_PUBLIC_URL":          "https://kb.example.com",
		"KB_DATABASE_URL":        "postgres://kb@db/kb",
		"KB_FGA_DATABASE_URL":    "postgres://kb@db/fga",
		"KB_RIVER_DATABASE_URL":  "postgres://kb@db/river",
		"KB_NATS_URL":            "nats://nats:4222",
		"KB_S3_ENDPOINT":         "http://seaweed:8333",
		"KB_S3_BUCKET":           "kb",
		"KB_S3_ACCESS_KEY":       "a",
		"KB_S3_SECRET_KEY":       "s",
		"KB_NODE_KEY_PASSPHRASE": "correct horse battery staple",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != RoleAll || c.Listen != ":8080" || c.SyncURL != c.PublicURL {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.SessionTTL != 15*time.Minute || c.RefreshTTL != 720*time.Hour {
		t.Fatalf("ttl defaults: %v %v", c.SessionTTL, c.RefreshTTL)
	}
	if c.Limits.BatchOps != 500 || c.Limits.QueryTimeout != 2*time.Second || c.Limits.RequestBodyBytes != 8<<20 {
		t.Fatalf("limits: %+v", c.Limits)
	}
	if !c.Secure() || c.PublicHost() != "kb.example.com" {
		t.Fatalf("url helpers: secure=%v host=%q", c.Secure(), c.PublicHost())
	}
}

func TestLoadCollectsAllErrors(t *testing.T) {
	_, err := Load(env(map[string]string{"KB_ROLE": "bogus", "KB_LOG_FORMAT": "xml"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"KB_ROLE", "KB_PUBLIC_URL", "KB_DATABASE_URL", "KB_FGA_DATABASE_URL", "KB_RIVER_DATABASE_URL", "KB_NATS_URL", "KB_S3_ENDPOINT", "node key custody", "KB_LOG_FORMAT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestFilesystemStoreSkipsS3(t *testing.T) {
	m := minimal()
	for _, k := range []string{"KB_S3_ENDPOINT", "KB_S3_BUCKET", "KB_S3_ACCESS_KEY", "KB_S3_SECRET_KEY"} {
		delete(m, k)
	}
	m["KB_OBJECT_STORE_DIR"] = "/var/lib/kb/objects"
	if _, err := Load(env(m)); err != nil {
		t.Fatal(err)
	}
}

func TestPlainHTTPNeedsDevOrLoopback(t *testing.T) {
	m := minimal()
	m["KB_PUBLIC_URL"] = "http://kb.internal"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected plain http to be refused")
	}
	m["KB_DEV"] = "true"
	if _, err := Load(env(m)); err != nil {
		t.Fatal(err)
	}
	m = minimal()
	m["KB_PUBLIC_URL"] = "http://localhost:8080/"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "http://localhost:8080" || c.Secure() {
		t.Fatalf("trailing slash should be trimmed and http is not secure: %+v", c.PublicURL)
	}
}

func TestLimitsOverrides(t *testing.T) {
	m := minimal()
	m["KB_LIMITS_ASSET_BYTES"] = "1GB"
	m["KB_LIMITS_BATCH_OPS"] = "100"
	m["KB_LIMITS_QUERY_TIMEOUT"] = "5s"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.AssetBytes != 1<<30 || c.Limits.BatchOps != 100 || c.Limits.QueryTimeout != 5*time.Second {
		t.Fatalf("overrides not applied: %+v", c.Limits)
	}
	m["KB_LIMITS_BATCH_OPS"] = "5000"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("batch ops above 500 must be refused")
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{"1024": 1024, "8MB": 8 << 20, "2gb": 2 << 30, "250 MiB": 250 << 20, "16kib": 16 << 10, "7B": 7}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Error("expected error")
	}
}
