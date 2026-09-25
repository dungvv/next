// Package gateway is the "macro gateway" subcommand: the websocket edge that
// tracks user connections and fans out realtime events.
//
// Port of services/connection_gateway (Rust). The Rust service kept
// connections in DynamoDB and bridged replicas through Redis pub/sub; here:
//
//   - ws endpoint via coder/websocket, JWT auth (golang-jwt)
//   - connection registry in Valkey (conn:<user_id> sets, 60s heartbeat TTL)
//     with a Postgres fallback (gateway_connections tables)
//   - cross-instance delivery via NATS core subjects realtime.user.* and
//     notifications.status.* — every replica subscribes, each writes to the
//     sockets it owns
//   - stale-connection cleanup as an in-process sweeper goroutine
//     (scripts/stale_connections.rs)
//   - an internal HTTP API on GATEWAY_INTERNAL_PORT for non-NATS callers
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/frecency"
	"github.com/macro-inc/macro/pkg/natsx"
)

// Server bundles the gateway's runtime state.
type Server struct {
	cfg      Config
	hub      *Hub
	reg      *Registry
	nc       *nats.Conn // may be nil if NATS is unreachable
	verifier TokenVerifier
	instance string // this replica's id, for logging/debug
	// frec records track_entity events; nil when Postgres is unavailable.
	frec *frecency.Store
	// lifeCtx cancels all open websocket connections on shutdown.
	lifeCtx context.Context
}

// Run starts the gateway: websocket listener on cfg.Port (GATEWAY_PORT
// override) and the internal API on GATEWAY_INTERNAL_PORT.
func Run(ctx context.Context, cfg config.Config) error {
	gcfg, err := loadConfig(cfg)
	if err != nil {
		return err
	}

	// --- Registry: Valkey primary, Postgres fallback. -----------------------
	rdb := redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr})
	primary := newValkeyRegistry(rdb, gcfg.ConnTTL)

	var fallback registryBackend
	pgPool, err := pgxpool.New(ctx, cfg.MacroDBURL)
	if err != nil {
		slog.Warn("gateway: macrodb pool init failed; registry has no PG fallback", "err", err)
	} else if err := pgPool.Ping(ctx); err != nil {
		slog.Warn("gateway: macrodb unreachable; registry has no PG fallback", "err", err)
		pgPool.Close()
		pgPool = nil
	}
	if pgPool != nil {
		if pr, err := newPGRegistry(pgPool, gcfg.ConnTTL); err != nil {
			slog.Warn("gateway: pg registry init failed", "err", err)
		} else {
			fallback = pr
		}
	}
	reg := newRegistry(primary, fallback, gcfg.ConnTTL)

	// --- NATS fanout. -------------------------------------------------------
	// natsx.Connect uses RetryOnFailedConnect, so nc may return while still
	// reconnecting; subscriptions re-register on reconnect.
	var nc *nats.Conn
	if c, _, err := natsx.Connect(ctx, cfg.NatsURL); err != nil {
		slog.Error("gateway: nats connect failed; local delivery only", "err", err)
	} else {
		nc = c
	}

	verifier, err := newVerifier(gcfg)
	if err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)

	s := &Server{
		cfg:      gcfg,
		hub:      newHub(gcfg.SendBuffer),
		reg:      reg,
		nc:       nc,
		verifier: verifier,
		instance: uuid.NewString(),
		lifeCtx:  gctx,
	}
	if pgPool != nil {
		s.frec = frecency.New(pgPool)
		// Port of connection_gateway's FrecencyAggregatorWorkerHandle
		// (60s poll): fold frecency_events into frecency_aggregates.
		g.Go(func() error {
			s.frec.RunAggregator(gctx, frecency.DefaultPollInterval)
			return nil
		})
	}

	if nc != nil {
		g.Go(func() error {
			if err := s.subscribeFanout(gctx, nc); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("fanout subscribe: %w", err)
			}
			return nil
		})
	}

	// --- HTTP servers. ------------------------------------------------------
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /{$}", s.handleWS)
	publicMux.HandleFunc("GET /connection-gateway", s.handleWS)
	publicMux.HandleFunc("GET /connection-gateway/", s.handleWS)
	publicMux.HandleFunc("GET /health", s.handleHealth)
	publicMux.HandleFunc("GET /healthz", s.handleHealth)

	publicSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", gcfg.Port),
		Handler:           publicMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	internalSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", gcfg.InternalPort),
		Handler:           s.internalMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	g.Go(func() error {
		slog.Info("gateway: websocket listening", "port", gcfg.Port)
		if err := publicSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("ws server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		slog.Info("gateway: internal api listening", "port", gcfg.InternalPort)
		if err := internalSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("internal server: %w", err)
		}
		return nil
	})

	// Sweeper: port of scripts/stale_connections.rs — reaps registry members
	// whose heartbeat TTL expired (crashed replicas, dead clients).
	g.Go(func() error {
		t := time.NewTicker(gcfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-t.C:
				sctx, cancel := context.WithTimeout(gctx, 30*time.Second)
				n, err := reg.Sweep(sctx)
				cancel()
				if err != nil {
					slog.Warn("gateway: sweep failed", "err", err)
				} else if n > 0 {
					slog.Info("gateway: swept stale connections", "removed", n)
				}
			}
		}
	})

	// Graceful shutdown.
	g.Go(func() error {
		<-gctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = publicSrv.Shutdown(shCtx)
		_ = internalSrv.Shutdown(shCtx)
		return nil
	})

	err = g.Wait()

	// Best-effort final cleanup.
	if nc != nil {
		nc.Drain()
	}
	rdb.Close()
	if pgPool != nil {
		pgPool.Close()
	}
	return err
}
