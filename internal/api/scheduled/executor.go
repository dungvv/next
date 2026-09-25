package scheduled

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// LiveUpdates publishes ScheduledActionUpdate payloads to the owner's live
// connections (replaces ConnectionGatewayClient::batch_send_message).
type LiveUpdates interface {
	PublishUpdate(update any) // ScheduledActionUpdateStarted | ScheduledActionUpdateStopped
}

// TaskRunner executes the action's task payload against the run chat. The
// Rust agent runner needs ai_tools + the agent loop (Anthropic); until those
// land it is stubbed and every run is recorded as a failed execution.
type TaskRunner interface {
	// Run executes the task; returning an error marks the run failed.
	Run(ctx context.Context, action ScheduledAction, chatID string) error
}

// UnimplementedTaskRunner records every run as failed. TODO(agent-loop): port
// crates/agent + ai_tools (AiHost::Chat) so scheduled agent tasks actually
// run, then notify completion via notifications.ingress.
type UnimplementedTaskRunner struct{}

func (UnimplementedTaskRunner) Run(context.Context, ScheduledAction, string) error {
	return errors.New("scheduled action agent runner not yet ported (ai_tools/agent)")
}

// InProcessExecutor mirrors outbound::inprocess_executor::InProcessExecutor:
// claim → create run chat → publish "started" → spawn the run → record +
// release + publish "stopped".
type InProcessExecutor struct {
	repo        *Repo
	db          *pgxpool.Pool
	runner      TaskRunner
	liveUpdates LiveUpdates
}

// NewInProcessExecutor builds the executor.
func NewInProcessExecutor(repo *Repo, db *pgxpool.Pool, runner TaskRunner, live LiveUpdates) *InProcessExecutor {
	return &InProcessExecutor{repo: repo, db: db, runner: runner, liveUpdates: live}
}

func tryClaim(a *ScheduledAction) error {
	if a.Claimed != nil && time.Since(*a.Claimed) < MaxActionTime {
		return &AlreadyRunningError{ActionID: a.ID}
	}
	return nil
}

// ExecuteAction implements Executor.
func (e *InProcessExecutor) ExecuteAction(ctx context.Context, a ScheduledAction) (InProgressExecution, error) {
	owner, err := a.OwnerUser()
	if err != nil {
		return InProgressExecution{}, err
	}
	if err := tryClaim(&a); err != nil {
		return InProgressExecution{}, err
	}
	if err := e.repo.ClaimAction(ctx, a.ID); err != nil {
		return InProgressExecution{}, err
	}

	// Create the chat up front so the caller gets a chat_id synchronously.
	chatID, err := e.createRunChat(ctx, &a)
	if err != nil {
		_ = e.repo.ReleaseAction(ctx, a.ID)
		return InProgressExecution{}, err
	}

	e.liveUpdates.PublishUpdate(ScheduledActionUpdateStarted{
		Type: "started", Owner: owner, ActionID: a.ID, ChatID: chatID,
	})

	execution := InProgressExecution{ActionID: a.ID, ChatID: &chatID}
	startTime := time.Now().UTC()
	go e.finishRun(a, chatID, owner, startTime)
	return execution, nil
}

func (e *InProcessExecutor) finishRun(a ScheduledAction, chatID, owner string, startTime time.Time) {
	// Runs outlive the request; give them their own bounded context.
	ctx, cancel := context.WithTimeout(context.Background(), MaxActionTime)
	defer cancel()

	runErr := e.runner.Run(ctx, a, chatID)
	endTime := time.Now().UTC()

	result := json.RawMessage(`null`)
	if runErr != nil {
		if b, merr := json.Marshal(runErr.Error()); merr == nil {
			result = b
		}
	}
	if err := e.repo.CreateExecutionRecord(ctx, ActionExecutionRecord{
		ID:         uuid.Nil,
		ActionID:   a.ID,
		ResourceID: &chatID,
		StartTime:  startTime,
		EndTime:    endTime,
		IsSuccess:  runErr == nil,
		Result:     result,
		CreatedAt:  endTime,
	}); err != nil {
		slog.Error("scheduled: failed to save execution record", "action_id", a.ID, "err", err)
	}
	if err := e.repo.UpdateLastExecuted(ctx, a.ID, endTime); err != nil {
		slog.Error("scheduled: failed to update last executed", "action_id", a.ID, "err", err)
	}
	if err := e.repo.UpdateNextRunAt(ctx, a.ID); err != nil {
		slog.Error("scheduled: failed to update next_run_at", "action_id", a.ID, "err", err)
	}
	// Release before the stopped update so a follow-up "run now" cannot race.
	if err := e.repo.ReleaseAction(ctx, a.ID); err != nil {
		slog.Error("scheduled: failed to release action claim", "action_id", a.ID, "err", err)
	}
	e.liveUpdates.PublishUpdate(ScheduledActionUpdateStopped{
		Type: "stopped", Owner: owner, ActionID: a.ID, ChatID: chatID, IsSuccess: runErr == nil,
	})
	if runErr != nil {
		slog.Error("scheduled: action execution failed", "action_id", a.ID, "err", runErr)
	}
}

// createRunChat creates the run transcript chat on macrodb, like
// agent_task::create_run_chat (PgChatRepo::create).
// TODO(share-permission): the Rust create also records a SharePermissionV2
// from the owner's team default; replicate once the permissions schema is ported.
func (e *InProcessExecutor) createRunChat(ctx context.Context, a *ScheduledAction) (string, error) {
	owner, err := a.OwnerUser()
	if err != nil {
		return "", err
	}
	var task AgentTask
	model := ""
	if err := json.Unmarshal(a.Task, &task); err == nil {
		model = task.Model
	}
	row, err := macrodb.New(e.db).CreateChatV2(ctx, macrodb.CreateChatV2Params{
		UserId:       owner,
		Name:         a.Name,
		Model:        model,
		ProjectId:    pgtype.Text{},
		IsPersistent: true,
	})
	if err != nil {
		return "", fmt.Errorf("create run chat: %w", err)
	}
	return row.ID, nil
}
