// Package chat implements the "macro chat" subcommand: the channels/messages
// HTTP service ported from crates/channels + crates/messages. It owns the
// comms schema, authenticates with the shared JWT/internal-key middleware,
// and publishes realtime frames (NATS core, realtime.user.<id>) plus
// notification ingress requests (JetStream notifications.ingress).
package chat

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/notification"
	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/frecency"
	"github.com/macro-inc/macro/pkg/natsx"
)

// Run starts the chat service.
func Run(ctx context.Context, cfg config.Config) error {
	ccfg, err := LoadConfig()
	if err != nil {
		return err
	}

	// ---- postgres (comms schema on the shared database) ----
	pool, err := pgxpool.New(ctx, cfg.CommsDBURL)
	if err != nil {
		return fmt.Errorf("chat: open comms db: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("chat: ping comms db: %w", err)
	}
	db := newStore(pool)

	// ---- NATS: realtime fanout (core) + notification ingress (JetStream) ----
	nc, js, err := natsx.Connect(ctx, cfg.NatsURL)
	if err != nil {
		return fmt.Errorf("chat: nats connect: %w", err)
	}
	defer nc.Close()
	// Producers ensure the work-queue stream exists so first-publish never
	// drops when the notification service hasn't booted yet.
	if err := notification.EnsureStreams(ctx, js); err != nil {
		return fmt.Errorf("chat: ensure notification stream: %w", err)
	}
	// Chat also publishes jobs.delete_chat on channel deletion.
	if _, err := natsx.EnsureEventStream(ctx, js, events.StreamJobs, []string{events.StreamJobs + ".>"}); err != nil {
		return fmt.Errorf("chat: ensure jobs stream: %w", err)
	}
	// Committed-post events for bot mention dispatch go to the
	// agent.trigger.<channel_id> shard stream consumed by agentharness.
	if _, err := natsx.EnsureEventStream(ctx, js, events.StreamAgentTrig, []string{events.StreamAgentTrig + ".>"}); err != nil {
		return fmt.Errorf("chat: ensure agent.trigger stream: %w", err)
	}
	fx := &effects{nc: nc, js: js}

	srv := NewServer(ccfg, db, fx, frecency.New(pool), auth.Middleware(ccfg.InternalAPIKey))
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", ccfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		slog.Info("chat listening", "port", ccfg.Port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shctx)
		return nil
	})
	return g.Wait()
}
