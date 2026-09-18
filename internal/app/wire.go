package app

import (
	"context"

	"connectrpc.com/connect"

	"github.com/acx1729/ocean/internal/api"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/keyring"
	"github.com/acx1729/ocean/internal/truth"
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

	if a.cfg.Role.RunsAPI() {
		a.services.Auth = auth.NewService(a.cfg, a.authStore, tokens, nil, a.log)
		a.services.Agents = auth.NewAgentsService(a.authStore, a.authn, nil, nil)
		svcs := api.New(api.Deps{
			Config: a.cfg, DB: a.db, Keys: a.keys, Truth: a.truth, Mat: a.mat, Guard: a.guard, AuthStore: a.authStore, Log: a.log,
		})
		a.services.Workspaces = svcs.Workspaces
		a.services.Projects = svcs.Projects
		a.services.Members = svcs.Members
		a.services.Schema = svcs.Schema
		a.services.Pages = svcs.Pages
		a.services.Blocks = svcs.Blocks
	}
	return nil
}

// interceptors returns the request-path interceptors in execution order.
func (a *App) interceptors() []connect.Interceptor {
	return []connect.Interceptor{auth.NewInterceptor(a.authn)}
}
