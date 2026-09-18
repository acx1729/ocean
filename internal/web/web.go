// Package web serves the embedded single-page app (web/, built by Vite into
// dist/) under a URL prefix. Hashed assets are immutable; every other path
// under the prefix falls back to index.html with the runtime configuration
// injected as a <meta name="kb-config"> tag, and a strict Content-Security-Policy.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Marker is replaced in index.html by the configuration tag.
const Marker = "<!--KB_CONFIG-->"

// Config is what the app learns about the node at load time.
type Config struct {
	PublicURL string `json:"publicUrl"`
	SyncURL   string `json:"syncUrl"`
	Version   string `json:"version"`
	Dev       bool   `json:"dev"`
	Wallets   bool   `json:"wallets"`
}

// Built reports whether a web build is embedded in this binary.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler serves the embedded build under prefix (for example "/app/").
func Handler(prefix string, cfg Config) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return NewHandler(sub, prefix, cfg)
}

// NewHandler serves a build from any file system (tests use fstest.MapFS).
func NewHandler(fsys fs.FS, prefix string, cfg Config) http.Handler {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return notBuilt(prefix)
	}
	rendered := bytes.Replace(index, []byte(Marker), []byte(configTag(cfg)), 1)
	files := http.FileServer(http.FS(fsys))
	csp := contentSecurityPolicy(cfg)
	modTime := time.Now()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel := strings.TrimPrefix(r.URL.Path, prefix)
		if rel != "" && rel != "index.html" && isFile(fsys, rel) {
			h := w.Header()
			if strings.HasPrefix(rel, "assets/") {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				h.Set("Cache-Control", "no-cache")
			}
			if strings.HasSuffix(rel, ".wasm") {
				h.Set("Content-Type", "application/wasm")
			}
			r2 := r.Clone(r.Context())
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = "/" + rel
			files.ServeHTTP(w, r2)
			return
		}
		// Everything else is a client-side route.
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", csp)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		http.ServeContent(w, r, "index.html", modTime, bytes.NewReader(rendered))
	})
}

func isFile(fsys fs.FS, name string) bool {
	name = path.Clean(name)
	if name == "." || strings.HasPrefix(name, "../") {
		return false
	}
	st, err := fs.Stat(fsys, name)
	return err == nil && !st.IsDir()
}

func configTag(cfg Config) string {
	b, _ := json.Marshal(cfg)
	return `<meta name="kb-config" content="` + html.EscapeString(string(b)) + `">`
}

// contentSecurityPolicy allows only same-origin resources, the WASM engine and
// the sync WebSocket (which may live on another host in split deployments).
func contentSecurityPolicy(cfg Config) string {
	connect := "'self'"
	if u, err := url.Parse(cfg.SyncURL); err == nil && u.Host != "" {
		if p, err := url.Parse(cfg.PublicURL); err != nil || p.Host != u.Host {
			scheme := "wss"
			if u.Scheme == "http" || u.Scheme == "ws" {
				scheme = "ws"
			}
			connect += " " + scheme + "://" + u.Host
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'wasm-unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src " + connect,
		"worker-src 'self' blob:",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

func notBuilt(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "<!doctype html><title>kb</title><body style=\"font:15px system-ui;padding:2rem\"><h1>Web app not built</h1><p>This binary was compiled without the web app. Run <code>make web</code> (or <code>cd web &amp;&amp; npm ci &amp;&amp; npm run build</code>) and rebuild, then open <code>%s</code>.</p></body>", html.EscapeString(prefix))
	})
}
