package app

import (
	"context"
	"net/http"
	"net/url"

	"connectrpc.com/connect"

	"github.com/acx1729/ocean/internal/api"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/keyring"
	kbsync "github.com/acx1729/ocean/internal/sync"
	"github.com/acx1729/ocean/internal/truth"
	"github.com/acx1729/ocean/internal/version"
)

// wire constructs the subsystems and services this role serves. Every service
// is registered here and nowhere else; the order follows the dependency graph.
func (a *App) wire(ctx context.Context) error {
	if err := a.loadNodeKey(ctx); err != nil {
		return err
	}
	a.keys = keyring.New(a.node, &keyStore{db: a.db, nodeDID: a.node.DID()}, keyring.Options{})

	tokens, err := auth.NewTokenIssuer(a.node, a.cfg.PublicURL, a.cfg.SessionTTL)
	if err != nil {
		return err
	}
	a.authStore = auth.NewStore(a.db)
	a.authn = auth.NewAuthenticator(auth.AuthenticatorOptions{
		Tokens: tokens, Store: a.authStore, Limiter: auth.NewPGLimiter(a.db), Limits: a.cfg.Limits,
		OperatorToken: a.operatorToken, TrustProxy: a.cfg.TrustProxy, Logger: a.log,
	})
	a.guard = authz.NewRoleGuard(a.authStore)
	a.truth = truth.NewStore(a.keys, a.log)
	a.mat = truth.NewMaterializer(a.truth, a.db, 256)

	// Sync rooms live in the sync role; with KB_ROLE=all they share the process
	// with the API, so API writes fan out to rooms directly.
	if a.cfg.Role.RunsSync() {
		a.hub = kbsync.NewHub(kbsync.Deps{
			DB: a.db, Mat: a.mat, Truth: a.truth, Authn: a.authn, Guard: a.guard, Limits: a.cfg.Limits, Log: a.log,
			SyncURL: a.cfg.SyncURL, Origins: a.wsOrigins(), Version: version.Version,
		})
		hubCtx, cancel := context.WithCancel(context.Background())
		a.hub.Start(hubCtx)
		a.closers = append(a.closers, func(context.Context) error { cancel(); a.hub.Close(); return nil })
		a.services.Sync = kbsync.NewService(a.hub)
		a.routes = append(a.routes, func(mux *http.ServeMux) { mux.Handle("/ws/sync", a.hub.Handler()) })
	}

	if a.cfg.Role.RunsAPI() {
		a.services.Auth = auth.NewService(a.cfg, a.authStore, tokens, nil, a.log)
		a.services.Agents = auth.NewAgentsService(a.authStore, a.authn, nil, nil)
		deps := api.Deps{
			Config: a.cfg, DB: a.db, Keys: a.keys, Truth: a.truth, Mat: a.mat, Guard: a.guard, AuthStore: a.authStore, Log: a.log,
		}
		if a.hub != nil {
			deps.Notify = a.hub.Broadcast
		}
		svcs := api.New(deps)
		a.services.Workspaces = svcs.Workspaces
		a.services.Projects = svcs.Projects
		a.services.Members = svcs.Members
		a.services.Schema = svcs.Schema
		a.services.Pages = svcs.Pages
		a.services.Blocks = svcs.Blocks
	}
	return nil
}

// wsOrigins lists the Origin hosts allowed to open the sync WebSocket: the
// public URL, the sync URL and, in development, anything.
func (a *App) wsOrigins() []string {
	var out []string
	for _, raw := range []string{a.cfg.PublicURL, a.cfg.SyncURL} {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	if a.cfg.Dev {
		out = append(out, "*")
	}
	return out
}

// interceptors returns the request-path interceptors in execution order.
func (a *App) interceptors() []connect.Interceptor {
	return []connect.Interceptor{auth.NewInterceptor(a.authn)}
}
