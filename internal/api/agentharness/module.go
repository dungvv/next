package agentharness

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	runtime "github.com/macro-inc/macro/pkg/runtime"
)

// Module is the assembled agentharness service: HTTP deps plus its
// background workers (the trigger consumer) and its runtime client.
type Module struct {
	Deps
	svc *Service
	rt  runtime.RuntimePort // nil when unarmed
	cfg Config
}

// New builds the module. A missing/unreachable Docker daemon leaves the
// module armed for external sessions but unarmed for managed spawns — the
// API still serves, managed creation fails loudly at spawn time.
func New(ctx context.Context, cfg Config, pool *pgxpool.Pool, js jetstream.JetStream) (*Module, error) {
	repo := NewRepository(pool)
	secrets := runtime.SecretPort(runtime.EnvSecrets{Prefix: cfg.SecretPrefix})

	var rt runtime.RuntimePort
	if cfg.RuntimeDriver == "docker" {
		d, err := runtime.NewDockerRuntime(ctx, runtime.DockerConfig{
			Host:        cfg.DockerHost,
			Network:     cfg.ContainerNetwork,
			Allowlist:   runtime.NewEnvAllowlist(cfg.allowlist()...),
			PullMissing: cfg.PullImages,
			Secrets:     secrets,
		})
		if err != nil {
			slog.Warn("agentharness: docker unavailable; managed sandboxes disabled", "err", err)
		} else {
			rt = d
		}
	}

	svc := NewService(repo, rt, secrets, cfg)
	return &Module{Deps: Deps{Svc: svc}, svc: svc, rt: rt, cfg: cfg}, nil
}

// StartWorkers launches the trigger consumer; a no-op without JetStream or
// when AGENT_TRIGGER_ENABLED=false.
func (m *Module) StartWorkers(ctx context.Context, js jetstream.JetStream) {
	if js == nil || !m.cfg.TriggerEnabled {
		return
	}
	go func() {
		if err := m.svc.RunTriggerConsumer(ctx, js); err != nil &&
			!errors.Is(err, context.Canceled) {
			slog.Error("agentharness: trigger consumer exited", "err", err)
		}
	}()
}

// Close releases the runtime client.
func (m *Module) Close() {
	if m.rt != nil {
		if err := m.rt.Close(); err != nil {
			slog.Warn("agentharness: runtime close failed", "err", err)
		}
	}
}
