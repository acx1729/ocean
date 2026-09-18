// Command kb is the single server binary of the knowledge platform. It has no
// command-line interface: KB_ROLE selects the services a process runs,
// migrations apply themselves at startup, and every operator action is an
// AdminService RPC (specification section 2).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/acx1729/ocean/internal/app"
	"github.com/acx1729/ocean/internal/config"
	"github.com/acx1729/ocean/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	log := logging.New(cfg.LogLevel, cfg.LogFormat, os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
