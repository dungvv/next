// Package syncsvc is the Go port skeleton for services/sync-service
// (the Rust/WASM Cloudflare Worker + Durable Object sync service), per
// docs/GO_SELFHOST_PLAN.md section "D. macro sync".
//
// Feasibility-spike status — see REPORT.md in this directory.
package syncsvc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/internal/syncsvc/lorowasm"
	"github.com/macro-inc/macro/pkg/config"
)

// Run starts the syncsvc service. It keeps the signature used by the service
// registry so `macro syncsvc` can slot in without touching cmd/.
func Run(ctx context.Context, cfg config.Config) error {
	scfg := loadSyncConfig()
	log := slog.Default()

	pool, err := pgxpool.New(ctx, cfg.MacroDBURL)
	if err != nil {
		return fmt.Errorf("syncsvc: connect postgres: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("syncsvc: ping postgres: %w", err)
	}

	store := newPGStore(pool)
	if err := store.ensureSchema(ctx); err != nil {
		return fmt.Errorf("syncsvc: ensure schema: %w", err)
	}

	factory, lrt, err := pickEngineFactory(ctx, scfg, func(f string, a ...any) {
		log.Warn(fmt.Sprintf(f, a...))
	})
	if err != nil {
		return fmt.Errorf("syncsvc: engine init: %w", err)
	}
	defer func() {
		if lrt != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = lrt.Close(cctx)
		}
	}()
	if lrt != nil {
		lrt.Logf = func(f string, a ...any) { log.Debug(fmt.Sprintf("lorowasm: "+f, a...)) }
	}

	mgr := newSessionManager(store, factory, scfg, log)
	srv := NewServer(mgr, scfg, log)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", scfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	srv.lifeCtx = ctx

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("syncsvc listening", "port", scfg.Port)
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
		mgr.closeAll()
		return nil
	})
	err = g.Wait()
	if lrt != nil {
		if stubs := lrt.StubReport(); len(stubs) > 0 {
			log.Warn("lorowasm stubbed imports invoked", "stubs", lorowasm.SortedKeys(stubs))
		}
	}
	return err
}
