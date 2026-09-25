// Package entrypoint provides the shared service bootstrap: structured
// logging (slog), OTel tracing (optional), and signal handling.
// Go equivalent of Rust's macro_entrypoint.
package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Run starts a service subcommand: installs slog, wires SIGINT/SIGTERM to
// context cancellation, then runs fn. It returns the process exit error.
func Run(serviceName string, fn func(ctx context.Context) error) error {
	initSlog()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("service starting", "service", serviceName)
	if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", serviceName, err)
	}
	slog.Info("service stopped", "service", serviceName)
	return nil
}

func initSlog() {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	var h slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(h))
}
