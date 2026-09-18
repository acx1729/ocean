// Package app assembles a node from its configuration: database, migrations,
// key custody, services and the HTTP server. cmd/kb is a thin shell around it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/server"
	"github.com/acx1729/ocean/internal/telemetry"
	"github.com/acx1729/ocean/internal/version"
)

// App is a running node.
type App struct {
	cfg      *config.Config
	log      *slog.Logger
	db       *db.DB
	handler  http.Handler
	closers  []func(context.Context) error
	schemaV  int64
	services server.Services
	routes   []func(*http.ServeMux)
	ready    []func(context.Context) error
}

// New opens the dependencies and builds the handler. It does not listen.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	a := &App{cfg: cfg, log: log}
	shutdownTrace, err := telemetry.Setup(ctx, "kb", version.Version, string(cfg.Role), cfg.OTELEndpoint)
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, shutdownTrace)

	database, err := db.Open(ctx, cfg.DatabaseURL, cfg.ReplicaDatabaseURL, db.Options{AppName: "kb-" + string(cfg.Role), Logger: log})
	if err != nil {
		a.close(ctx)
		return nil, fmt.Errorf("database: %w", err)
	}
	a.db = database
	a.closers = append(a.closers, func(context.Context) error { database.Close(); return nil })
	a.ready = append(a.ready, database.Ping)

	if cfg.Migrate {
		v, err := database.Migrate(ctx)
		if err != nil {
			a.close(ctx)
			return nil, err
		}
		a.schemaV = v
		log.Info("schema ready", "version", v)
	}

	if err := a.wire(ctx); err != nil {
		a.close(ctx)
		return nil, err
	}

	reg := telemetry.NewRegistry()
	interceptors := []connect.Interceptor{server.NewMetrics(reg)}
	interceptors = append(interceptors, a.interceptors()...)
	a.handler = server.New(server.Deps{
		Config:       cfg,
		Logger:       log,
		Services:     a.services,
		Interceptors: interceptors,
		Ready:        a.readiness,
		Routes:       a.routes,
		Registry:     reg,
	})
	return a, nil
}

// Handler returns the root HTTP handler (used by tests).
func (a *App) Handler() http.Handler { return a.handler }

// DB exposes the database (used by tests and the bootstrap flow).
func (a *App) DB() *db.DB { return a.db }

func (a *App) readiness(ctx context.Context) error {
	var errs []error
	for _, r := range a.ready {
		if err := r(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run serves until ctx is cancelled, then drains connections.
func (a *App) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	ln, err := listen(a.cfg.Listen)
	if err != nil {
		return err
	}
	a.log.Info("listening", "addr", ln.Addr().String(), "role", a.cfg.Role, "public_url", a.cfg.PublicURL, "version", version.String())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.log.Info("shutting down")
	err = srv.Shutdown(shutdownCtx)
	a.close(shutdownCtx)
	return err
}

func (a *App) close(ctx context.Context) {
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](ctx); err != nil {
			a.log.Warn("shutdown", "error", err)
		}
	}
	a.closers = nil
}

func listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "unix://") {
		return net.Listen("unix", strings.TrimPrefix(addr, "unix://"))
	}
	return net.Listen("tcp", addr)
}
