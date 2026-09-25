package agentharness

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
	runtime "github.com/macro-inc/macro/pkg/runtime"
)

// SidecarPort mirrors provision::SIDECAR_PORT — the ACP sidecar's port
// inside a managed sandbox.
const SidecarPort = 8700

// SessionTokenVariable / EgressURLVariable mirror domain::model's env names.
const (
	SessionTokenVariable = "MACRO_SESSION_TOKEN"
	EgressURLVariable    = "MACRO_EGRESS_URL"
)

// Errors surfaced to the HTTP layer.
var (
	ErrNotFound    = errors.New("agentharness: session not found")
	ErrForbidden   = errors.New("agentharness: insufficient access")
	ErrBotRequired = errors.New("botId is required: no deployment default bot configured")
	ErrUnknownBot  = errors.New("unknown bot")
	ErrNotAgentBot = errors.New("bot is not an agent bot")
	ErrNotYourBot  = errors.New("caller may not open sessions for this bot")
	ErrUnarmed     = errors.New("managed runtime is not configured")
	ErrInvalidSize = errors.New("invalid sandbox size")
)

// Service orchestrates session persistence and runtime lifecycle — the port
// of agent_session's service plus agent_harness's managed-container paths,
// trimmed to the usable core (see package doc for parity gaps).
type Service struct {
	repo     *Repository
	rt       runtime.RuntimePort // nil when RuntimeDriver != docker
	secrets  runtime.SecretPort
	cfg      Config
	registry *Registry
	queues   *queueStore
}

// NewService wires the service. rt may be nil (unarmed); secrets may be
// NoopSecrets.
func NewService(repo *Repository, rt runtime.RuntimePort, secrets runtime.SecretPort, cfg Config) *Service {
	return &Service{
		repo:     repo,
		rt:       rt,
		secrets:  secrets,
		cfg:      cfg,
		registry: NewRegistry(),
		queues:   newQueueStore(),
	}
}

// Registry exposes the harness-connection registry to the WS gateway.
func (s *Service) Registry() *Registry { return s.registry }

// ---------- create --------------------------------------------------------

// Create opens a session. workspace set → external (caller-run runtime);
// absent → managed (this deployment provisions a sandbox through RuntimePort).
func (s *Service) Create(ctx context.Context, caller auth.Caller, req CreateSessionRequest) (*Session, error) {
	owner := caller.UserID
	if req.Owner != nil && *req.Owner != "" {
		// Bot principals may claim an owner; user callers always own their
		// own sessions. Trusted-header auth cannot verify the claim beyond
		// this — TODO(identity) when Casdoor validation lands.
		if !caller.Internal && strings.HasPrefix(caller.UserID, "macro|") {
			// user caller: ignore the claim entirely (Rust does the same)
		} else {
			owner = *req.Owner
		}
	}
	if !strings.HasPrefix(owner, "macro|") && !strings.HasPrefix(owner, "bot|") {
		return nil, fmt.Errorf("%w: owner %q is not a macro principal", ErrForbidden, owner)
	}

	// A bot is always required: managed requests fall back to the
	// deployment's configured bot; external requests must name one (Rust
	// lets bot callers omit it and use their own identity — bot-principal
	// auth is not ported, so the field is mandatory here).
	botID := s.cfg.BotID
	if req.BotID != nil && *req.BotID != "" {
		botID = *req.BotID
	}
	if botID == "" {
		return nil, ErrBotRequired
	}

	facts, err := botFacts(ctx, s.repo.db, botID)
	if err != nil {
		return nil, err
	}
	if facts == nil {
		return nil, ErrUnknownBot
	}
	if !facts.HasAgent {
		return nil, ErrNotAgentBot
	}
	// Ownership/team check for persisted bots: the caller must own the
	// bot or be internal. Team-membership check is a TODO (team_members
	// lookup belongs to the teams domain once ported).
	if facts.OwnerUserID != nil && *facts.OwnerUserID != caller.UserID && !caller.Internal {
		return nil, ErrNotYourBot
	}

	id := uuid.NewString()
	params := CreateParams{
		ID:           id,
		OwnerID:      owner,
		BotID:        botID,
		Model:        s.cfg.Model,
		Harness:      s.cfg.HarnessSlug,
		RepoURL:      req.RepoURL,
		Workspace:    s.cfgWorkspace(req),
		SandboxSize:  SandboxDefault,
		Instructions: req.Instructions,
	}
	if facts.ConfigHarness != nil {
		params.Harness = *facts.ConfigHarness
	}
	if req.RepoBranch != nil {
		params.RepoBranch = req.RepoBranch
	}
	if req.Thread != nil {
		t := req.Thread
		threadID := t.ThreadID
		if threadID == nil {
			threadID = &t.MessageID
		}
		params.ThreadID = threadID
		params.OriginatingMessageID = &t.MessageID
		if t.ParentType != "" && t.ParentID != nil {
			params.ThreadParentType = &t.ParentType
			params.ThreadParentID = t.ParentID
		} else if t.ChannelID != nil {
			ch := "channel"
			params.ThreadParentType = &ch
			params.ThreadParentID = t.ChannelID
		}
		// Channel members may view/drive the session (Rust grants the
		// originating channel edit access).
		if params.ThreadParentType != nil && *params.ThreadParentType == "channel" {
			params.ChannelGrant = params.ThreadParentID
		}
	}
	if size, err := s.repo.UserSandboxSize(ctx, owner); err == nil && size.valid() {
		params.SandboxSize = size
	}

	sess, err := s.repo.Create(ctx, params)
	if errors.Is(err, ErrThreadSessionExists) && params.ThreadID != nil {
		existing, gerr := s.repo.FindForThread(ctx, *params.ThreadID, botID)
		if gerr != nil {
			return nil, gerr
		}
		return nil, &ThreadExistsError{Session: existing}
	}
	if err != nil {
		return nil, err
	}

	// A bot configured for a user-run harness (agent_configs.harness_id, the
	// 'macrod' kind) is served by that harness's dialed-in connection, never
	// by a managed sandbox — even on a managed-shaped request.
	if facts.ConfigHarnessID != nil {
		s.registry.BindSession(sess.ID, *facts.ConfigHarnessID)
		slog.Info("agentharness: session bound to harness",
			"session", sess.ID, "harness", *facts.ConfigHarnessID)
		return sess, nil
	}

	if req.Workspace == nil {
		// Managed session: provision the sandbox. Failure to spawn fails the
		// request but keeps the row (status stays 'no_messages') so a retry
		// can re-spawn — mirroring Rust's durable-first ordering.
		if serr := s.provisionSandbox(ctx, sess); serr != nil {
			slog.Error("agentharness: sandbox spawn failed", "session", sess.ID, "err", serr)
			_ = s.repo.SetStatus(ctx, sess.ID, "disconnected", nil)
			return sess, fmt.Errorf("session created but sandbox failed to start: %w", serr)
		}
	} else {
		// External session on a caller-run runtime with no harness binding:
		// recorded; work routes when a runtime claims it.
		slog.Info("agentharness: external session recorded", "session", sess.ID)
	}
	return sess, nil
}

// ThreadExistsError carries the session a thread already routes to.
type ThreadExistsError struct{ Session *Session }

func (e *ThreadExistsError) Error() string {
	return ErrThreadSessionExists.Error()
}

func (s *Service) cfgWorkspace(req CreateSessionRequest) string {
	if req.Workspace != nil {
		return *req.Workspace
	}
	return "/workspace"
}

// provisionSandbox spawns the session's container and brings it ready.
func (s *Service) provisionSandbox(ctx context.Context, sess *Session) error {
	if s.rt == nil {
		return ErrUnarmed
	}
	// The sandbox's one secret: an opaque session token minted here, stored
	// only as a hash (egress_token_hash) — a DB dump must not yield it.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := "mst_" + hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))

	env := map[string]string{
		SessionTokenVariable: token,
		// The model credential resolves through the secret store, never from
		// the caller's request or the process env directly.
		"ANTHROPIC_API_KEY": "secret:ANTHROPIC_API_KEY",
	}
	if s.cfg.EgressURL != "" {
		env[EgressURLVariable] = s.cfg.EgressURL
	}
	if _, err := s.secrets.Resolve(ctx, "ANTHROPIC_API_KEY"); err != nil {
		// Fail loudly before spawning: a credential-less agent is dead weight.
		return fmt.Errorf("managed sandbox needs ANTHROPIC_API_KEY secret: %w", err)
	}

	name := "macro-agent-" + sess.ID
	_, err := s.rt.Spawn(ctx, runtime.TaskSpec{
		Name:  name,
		Image: s.cfg.ContainerImage,
		Env:   env,
		Labels: map[string]string{
			runtime.SessionLabel: sess.ID,
		},
		Network: s.cfg.ContainerNetwork,
		Limits:  sess.SandboxSize.limits(),
	})
	if err != nil {
		return err
	}
	if err := s.repo.SetEgressTokenHash(ctx, sess.ID, hex.EncodeToString(hash[:])); err != nil {
		return err
	}

	// Bring the sandbox ready: clone the workspace through egress and start
	// the ACP sidecar. Ported from container/ensure_ready.sh — idempotent, so
	// re-running after a restart is safe.
	ctx2, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	res, err := s.rt.Exec(ctx2, name, []string{ensureReadyScript}, 0)
	if err != nil {
		return fmt.Errorf("ensure-ready exec: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ensure-ready exited %d: %s", res.ExitCode, truncate(res.Output, 512))
	}
	return nil
}

// ensureReadyScript is the Go port of crates/agent_harness/container/
// ensure_ready.sh, minus the baked nix dev shell (self-host images do not
// carry it).
const ensureReadyScript = `set -e
workspace_dir=/workspace
sidecar_port=8700
sidecar_log=/tmp/acp-sidecar.log
if [ ! -d "$workspace_dir/.git" ] && [ -n "$MACRO_EGRESS_URL" ]; then
  egress_git_url="${MACRO_EGRESS_URL%/}/git"
  git config --global "credential.${MACRO_EGRESS_URL}.helper" \
    '!f() { echo username=x-access-token; echo "password=$MACRO_SESSION_TOKEN"; }; f'
  git clone --depth 1 "$egress_git_url" "$workspace_dir" || true
fi
if [ ! -x /opt/acp-sidecar ]; then
  echo "no executable /opt/acp-sidecar in this image" >&2
  exit 1
fi
if ! curl -sf "localhost:$sidecar_port/ping" >/dev/null 2>&1; then
  nohup /opt/acp-sidecar >"$sidecar_log" 2>&1 &
fi
`

// ---------- read / control ------------------------------------------------

// GetForCaller loads a session when the caller may see it.
func (s *Service) GetForCaller(ctx context.Context, caller auth.Caller, id string) (*Session, string, error) {
	sess, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if sess == nil {
		return nil, "", ErrNotFound
	}
	level, err := s.repo.AccessLevel(ctx, id, caller.UserID)
	if err != nil {
		return nil, "", err
	}
	if level == "" && !caller.Internal {
		return nil, "", ErrForbidden
	}
	if caller.Internal && level == "" {
		level = "owner"
	}
	return sess, level, nil
}

// requireEdit returns the session when the caller may drive it.
func (s *Service) requireEdit(ctx context.Context, caller auth.Caller, id string) (*Session, error) {
	sess, level, err := s.GetForCaller(ctx, caller, id)
	if err != nil {
		return nil, err
	}
	if level != "owner" && level != "edit" {
		return nil, ErrForbidden
	}
	return sess, nil
}

// Delete stops the sandbox (managed) and removes the row.
func (s *Service) Delete(ctx context.Context, caller auth.Caller, id string) error {
	sess, err := s.requireEdit(ctx, caller, id)
	if err != nil {
		return err
	}
	if s.rt != nil && sess.External == nil {
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if task, _ := s.rt.FindByLabel(ctx2, runtime.SessionLabel, sess.ID); task != nil {
			if rerr := s.rt.Remove(ctx2, task.ID); rerr != nil {
				slog.Warn("agentharness: failed to remove sandbox", "session", id, "err", rerr)
			}
		}
	}
	return s.repo.Delete(ctx, id)
}

// Control dispatches one action to the session's runtime, or queues it when
// a turn is running / the runtime is unreachable. Returns the disposition.
func (s *Service) Control(ctx context.Context, caller auth.Caller, id string, req ControlRequest) (ControlResponse, error) {
	sess, err := s.requireEdit(ctx, caller, id)
	if err != nil {
		return ControlResponse{}, err
	}
	actionID := uuid.NewString()
	if req.ActionID != nil && *req.ActionID != "" {
		actionID = *req.ActionID
	}
	if err := s.dispatch(ctx, sess, actionID, caller.UserID, req.Action); err != nil {
		if errors.Is(err, errRuntimeBusy) {
			s.queues.add(id, QueuedAction{
				ActionID: actionID, UserID: caller.UserID,
				Action: req.Action, QueuedAt: time.Now().UTC(),
			})
			return ControlResponse{ActionID: actionID, Status: "queued"}, nil
		}
		return ControlResponse{}, err
	}
	return ControlResponse{ActionID: actionID, Status: "sent"}, nil
}

var errRuntimeBusy = errors.New("runtime busy or unreachable; action queued")

// dispatch sends the action to whichever runtime serves the session.
// TODO(acp): the managed path must speak the full ACP envelope through the
// sidecar; today it forwards the action as a session/prompt-shaped JSON-RPC
// notification and records it in the session log.
func (s *Service) dispatch(ctx context.Context, sess *Session, actionID, userID string, action map[string]any) error {
	frame, _ := json.Marshal(map[string]any{
		"type": "acp", "actionId": actionID, "action": action,
	})
	if err := s.repo.AppendLog(ctx, uuid.NewString(), sess.ID, &userID, "to_runtime", frame); err != nil {
		slog.Warn("agentharness: failed to log control frame", "session", sess.ID, "err", err)
	}

	if sess.External != nil {
		// Externally-served session: forward through the harness connection
		// when its bot is bound to a dialed-in runtime.
		if s.registry.Send(sess.ID, frame) {
			return nil
		}
		return errRuntimeBusy
	}
	// Managed session: forward to the sidecar over the container network.
	if s.rt == nil {
		return errRuntimeBusy
	}
	task, err := s.rt.FindByLabel(ctx, runtime.SessionLabel, sess.ID)
	if err != nil || task == nil {
		return errRuntimeBusy
	}
	if err := sendToSidecar(ctx, task.Name, frame); err != nil {
		slog.Warn("agentharness: sidecar send failed", "session", sess.ID, "err", err)
		return errRuntimeBusy
	}
	return nil
}

// Queue returns the session's pending actions, oldest first.
func (s *Service) Queue(ctx context.Context, caller auth.Caller, id string) ([]QueuedAction, error) {
	if _, _, err := s.GetForCaller(ctx, caller, id); err != nil {
		return nil, err
	}
	return s.queues.list(id), nil
}

// EditQueuedAction replaces a queued prompt's text.
func (s *Service) EditQueuedAction(ctx context.Context, caller auth.Caller, id, actionID, prompt string) error {
	if _, err := s.requireEdit(ctx, caller, id); err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt must not be blank")
	}
	if !s.queues.edit(id, actionID, prompt) {
		return ErrNotFound
	}
	return nil
}

// RemoveQueuedAction drops a queued action.
func (s *Service) RemoveQueuedAction(ctx context.Context, caller auth.Caller, id, actionID string) error {
	if _, err := s.requireEdit(ctx, caller, id); err != nil {
		return err
	}
	if !s.queues.remove(id, actionID) {
		return ErrNotFound
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
