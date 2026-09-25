// Command macro is the all-in-one self-hosted backend.
// Each subcommand runs one service; `macro all` runs them in-process.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/macro-inc/macro/internal/api"
	"github.com/macro-inc/macro/internal/chat"
	"github.com/macro-inc/macro/internal/gateway"
	"github.com/macro-inc/macro/internal/jobs"
	"github.com/macro-inc/macro/internal/migrate"
	"github.com/macro-inc/macro/internal/notification"
	"github.com/macro-inc/macro/internal/scheduler"
	"github.com/macro-inc/macro/internal/syncsvc"
	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/entrypoint"
	"golang.org/x/sync/errgroup"
)

func migrationsDir() string {
	if d := os.Getenv("MIGRATIONS_DIR"); d != "" {
		return d
	}
	return "crates/macro_db_client/migrations"
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: macro <command>

  api          HTTP routers (auth, dss, dcs, email, calendar, ...)
  chat         chat service (channels + messages, comms DB)
  worker       JetStream consumers (ex-Lambda + ex-Kafka)
  gateway      websocket gateway (connection registry + NATS fanout)
  sync         CRDT sync service
  scheduler    periodic jobs (ex-EventBridge cron)
  notification notification ingress/delivery service
  migrate      apply sqlx-format migrations to all databases
  all          run every service in one process`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	services := map[string]func(context.Context, config.Config) error{
		"api":          api.Run,
		"chat":         chat.Run,
		"worker":       jobs.Run,
		"gateway":      gateway.Run,
		"sync":         syncsvc.Run,
		"scheduler":    scheduler.Run,
		"notification": notification.Run,
	}

	cmd := os.Args[1]
	if cmd == "migrate" {
		dir := migrationsDir()
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		err := entrypoint.Run("migrate", func(ctx context.Context) error {
			return migrate.Run(ctx, cfg, dir)
		})
		if err != nil {
			slog.Error("migrate", "err", err)
			os.Exit(1)
		}
		return
	}

	if cmd == "all" {
		err := entrypoint.Run("all", func(ctx context.Context) error {
			if err := migrate.Run(ctx, cfg, migrationsDir()); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			g, ctx := errgroup.WithContext(ctx)
			for name, fn := range services {
				g.Go(func() error {
					slog.Info("starting", "service", name)
					return fn(ctx, cfg)
				})
			}
			return g.Wait()
		})
		if err != nil {
			slog.Error("all", "err", err)
			os.Exit(1)
		}
		return
	}

	fn, ok := services[cmd]
	if !ok {
		usage()
	}
	if err := entrypoint.Run(cmd, func(ctx context.Context) error { return fn(ctx, cfg) }); err != nil {
		slog.Error(cmd, "err", err)
		os.Exit(1)
	}
}
