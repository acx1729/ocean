package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><head>" + Marker + "</head><body><div id=root></div></body>")},
		"assets/app-abc.js":   {Data: []byte("console.log(1)")},
		"assets/loro-x.wasm":  {Data: []byte{0, 'a', 's', 'm'}},
		".vite/manifest.json": {Data: []byte("{}")},
	}
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSPAFallbackInjectsConfigAndCSP(t *testing.T) {
	h := NewHandler(testFS(), "/app/", Config{PublicURL: "https://kb.example.com", SyncURL: "https://kb.example.com", Version: "1.2.3", Dev: false, Wallets: true})
	for _, p := range []string{"/app/", "/app/w/123/p/456/page/789", "/app/index.html", "/app/missing.js"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `<meta name="kb-config" content="{&#34;publicUrl&#34;:&#34;https://kb.example.com&#34;`) {
			t.Fatalf("%s: config not injected: %s", p, body)
		}
		if strings.Contains(body, Marker) {
			t.Fatalf("%s: marker left in place", p)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self' 'wasm-unsafe-eval'") || !strings.Contains(csp, "connect-src 'self';") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: unexpected CSP %q", p, csp)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: cache-control %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}

func TestAssetsAreImmutableAndTyped(t *testing.T) {
	h := NewHandler(testFS(), "/app", Config{PublicURL: "http://127.0.0.1:8080", SyncURL: "http://127.0.0.1:8080"})
	rec := get(t, h, "/app/assets/app-abc.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset: %d %q", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset cache-control %q", cc)
	}
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatalf("assets should not carry the document CSP")
	}
	rec = get(t, h, "/app/assets/loro-x.wasm")
	if ct := rec.Header().Get("Content-Type"); ct != "application/wasm" {
		t.Fatalf("wasm content-type %q", ct)
	}
	if rec := get(t, h, "/app/../etc/passwd"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kb-config") {
		t.Fatalf("traversal should fall back to the app shell: %d", rec.Code)
	}
}

func TestSplitSyncHostIsAllowedByCSP(t *testing.T) {
	h := NewHandler(testFS(), "/app/", Config{PublicURL: "https://kb.example.com", SyncURL: "https://sync.example.com"})
	csp := get(t, h, "/app/").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' wss://sync.example.com") {
		t.Fatalf("CSP %q", csp)
	}
}

func TestNotBuilt(t *testing.T) {
	h := NewHandler(fstest.MapFS{}, "/app/", Config{})
	rec := get(t, h, "/app/")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "make web") {
		t.Fatalf("not built: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	NewHandler(testFS(), "/app/", Config{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/app/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}
