// Package server assembles the HTTP surface of a node: Connect handlers for
// every kb.v1 service under /rpc, health and metrics endpoints, and the extra
// routes (sync WebSocket, public pages, MCP, OAuth, web app) contributed by
// other packages.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/version"
)

// Services lists the Connect service implementations to mount. Nil entries are
// not registered, which lets a role serve only the services it owns.
type Services struct {
	Auth         kbv1connect.AuthServiceHandler
	Agents       kbv1connect.AgentsServiceHandler
	Workspaces   kbv1connect.WorkspacesServiceHandler
	Members      kbv1connect.MembersServiceHandler
	Groups       kbv1connect.GroupsServiceHandler
	ShareLinks   kbv1connect.ShareLinksServiceHandler
	Projects     kbv1connect.ProjectsServiceHandler
	Permissions  kbv1connect.PermissionsServiceHandler
	Policy       kbv1connect.PolicyServiceHandler
	Schema       kbv1connect.SchemaServiceHandler
	Pages        kbv1connect.PagesServiceHandler
	Blocks       kbv1connect.BlocksServiceHandler
	Assets       kbv1connect.AssetsServiceHandler
	Query        kbv1connect.QueryServiceHandler
	Publications kbv1connect.PublicationsServiceHandler
	Sync         kbv1connect.SyncServiceHandler
	Events       kbv1connect.EventsServiceHandler
	Admin        kbv1connect.AdminServiceHandler
}

// Deps are the inputs to New.
type Deps struct {
	Config       *config.Config
	Logger       *slog.Logger
	Services     Services
	Interceptors []connect.Interceptor
	// Ready reports whether the node can serve traffic (database reachable,
	// node key unsealed). It backs /readyz.
	Ready func(ctx context.Context) error
	// Routes add non-Connect handlers to the mux.
	Routes   []func(mux *http.ServeMux)
	Registry *prometheus.Registry
}

// New builds the root handler.
func New(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Registry == nil {
		d.Registry = prometheus.NewRegistry()
	}
	rpc := http.NewServeMux()
	opts := []connect.HandlerOption{connect.WithInterceptors(d.Interceptors...)}
	mount := func(path string, h http.Handler) { rpc.Handle(path, h) }
	s := d.Services
	if s.Auth != nil {
		mount(kbv1connect.NewAuthServiceHandler(s.Auth, opts...))
	}
	if s.Agents != nil {
		mount(kbv1connect.NewAgentsServiceHandler(s.Agents, opts...))
	}
	if s.Workspaces != nil {
		mount(kbv1connect.NewWorkspacesServiceHandler(s.Workspaces, opts...))
	}
	if s.Members != nil {
		mount(kbv1connect.NewMembersServiceHandler(s.Members, opts...))
	}
	if s.Groups != nil {
		mount(kbv1connect.NewGroupsServiceHandler(s.Groups, opts...))
	}
	if s.ShareLinks != nil {
		mount(kbv1connect.NewShareLinksServiceHandler(s.ShareLinks, opts...))
	}
	if s.Projects != nil {
		mount(kbv1connect.NewProjectsServiceHandler(s.Projects, opts...))
	}
	if s.Permissions != nil {
		mount(kbv1connect.NewPermissionsServiceHandler(s.Permissions, opts...))
	}
	if s.Policy != nil {
		mount(kbv1connect.NewPolicyServiceHandler(s.Policy, opts...))
	}
	if s.Schema != nil {
		mount(kbv1connect.NewSchemaServiceHandler(s.Schema, opts...))
	}
	if s.Pages != nil {
		mount(kbv1connect.NewPagesServiceHandler(s.Pages, opts...))
	}
	if s.Blocks != nil {
		mount(kbv1connect.NewBlocksServiceHandler(s.Blocks, opts...))
	}
	if s.Assets != nil {
		mount(kbv1connect.NewAssetsServiceHandler(s.Assets, opts...))
	}
	if s.Query != nil {
		mount(kbv1connect.NewQueryServiceHandler(s.Query, opts...))
	}
	if s.Publications != nil {
		mount(kbv1connect.NewPublicationsServiceHandler(s.Publications, opts...))
	}
	if s.Sync != nil {
		mount(kbv1connect.NewSyncServiceHandler(s.Sync, opts...))
	}
	if s.Events != nil {
		mount(kbv1connect.NewEventsServiceHandler(s.Events, opts...))
	}
	if s.Admin != nil {
		mount(kbv1connect.NewAdminServiceHandler(s.Admin, opts...))
	}

	mux := http.NewServeMux()
	// Connect, gRPC-Web and JSON at /rpc/kb.v1.<Service>/<Method>; plain gRPC
	// clients that cannot set a path prefix use the same handlers at the root.
	mux.Handle("/rpc/", http.StripPrefix("/rpc", rpc))
	mux.Handle("/kb.v1.", rpc)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version.Version, "role": d.Config.Role})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := d.Ready(ctx); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.Handle("/metrics", promhttp.HandlerFor(d.Registry, promhttp.HandlerOpts{}))
	for _, r := range d.Routes {
		r(mux)
	}

	var h http.Handler = mux
	h = maxBytes(h, d.Config.Limits.RequestBodyBytes)
	h = securityHeaders(h, d.Config.Secure())
	h = accessLog(h, d.Logger)
	h = requestID(h)
	h = recovery(h, d.Logger)
	return h2c.NewHandler(h, &http2.Server{})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBytes caps request bodies except on upload routes, which enforce their own limit.
func maxBytes(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limit > 0 && r.Body != nil && !strings.HasPrefix(r.URL.Path, "/upload/") && !strings.HasPrefix(r.URL.Path, "/ws/") {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler, secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if secure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
