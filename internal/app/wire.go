package app

import (
	"context"

	"connectrpc.com/connect"

	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/keyring"
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
	if a.cfg.Role.RunsAPI() {
		a.services.Auth = auth.NewService(a.cfg, a.authStore, tokens, nil, a.log)
		a.services.Agents = auth.NewAgentsService(a.authStore, a.authn, nil, nil)
	}
	return nil
}

// interceptors returns the request-path interceptors in execution order.
func (a *App) interceptors() []connect.Interceptor {
	return []connect.Interceptor{auth.NewInterceptor(a.authn)}
}
