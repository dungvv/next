// Package scheduled ports services/scheduled_action: user-owned cron-scheduled
// actions stored in macrodb, dispatched by an in-process ticker (plus an
// optional JetStream `jobs.scheduled_due` trigger) with live updates pushed
// over NATS `realtime.user.<id>`.
package scheduled

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MaxActionTime bounds a claim: a `claimed` timestamp older than this is
// treated as abandoned and the action can be claimed again.
const MaxActionTime = 20 * time.Minute

// ActionKind mirrors domain::models::ActionKind.
type ActionKind string

const (
	ActionKindAgent ActionKind = "Agent"
)

// AgentTask mirrors domain::models::AgentTask.
type AgentTask struct {
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	UserPrompt string `json:"user_prompt"`
}

// CreateScheduledAction is the client-supplied create payload.
type CreateScheduledAction struct {
	Name     string          `json:"name"`
	Schedule string          `json:"schedule"`
	Kind     ActionKind      `json:"kind"`
	Timezone string          `json:"timezone"`
	Task     json.RawMessage `json:"task"`
	Enabled  bool            `json:"enabled"`
}

// UpdateScheduledAction is the client-supplied update payload.
type UpdateScheduledAction struct {
	Name     string          `json:"name"`
	Schedule string          `json:"schedule"`
	Kind     ActionKind      `json:"kind"`
	Timezone string          `json:"timezone"`
	Task     json.RawMessage `json:"task"`
	Enabled  bool            `json:"enabled"`
}

// ScheduledAction mirrors domain::models::ScheduledAction. `Owner` holds the
// principal string ("macro|<email>" today); the Rust `Owner` type also admits
// bot/team principals — kept as a string here for the same reason.
type ScheduledAction struct {
	ID        uuid.UUID       `json:"id"`
	Owner     string          `json:"owner"`
	Name      string          `json:"name"`
	Schedule  string          `json:"schedule"`
	Kind      ActionKind      `json:"kind"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Timezone  string          `json:"timezone"`
	Task      json.RawMessage `json:"task"`
	Claimed   *time.Time      `json:"claimed"`
	NextRunAt time.Time       `json:"next_run_at"`
	Enabled   bool            `json:"enabled"`
}

// OwnerUser returns the owner principal when it is a user, mirroring
// ScheduledAction::owner_user / OwnerNotUserError.
func (a *ScheduledAction) OwnerUser() (string, error) {
	if len(a.Owner) >= 6 && a.Owner[:6] == "macro|" {
		return a.Owner, nil
	}
	return "", &OwnerNotUserError{Owner: a.Owner}
}

// InProgressExecution mirrors domain::models::InProgressExecution.
type InProgressExecution struct {
	ActionID uuid.UUID `json:"action_id"`
	ChatID   *string   `json:"chat_id"`
}

// ActionExecutionRecord mirrors domain::models::ActionExecutionRecord.
type ActionExecutionRecord struct {
	ID         uuid.UUID       `json:"id"`
	ActionID   uuid.UUID       `json:"action_id"`
	ResourceID *string         `json:"resource_id"`
	StartTime  time.Time       `json:"start_time"`
	EndTime    time.Time       `json:"end_time"`
	IsSuccess  bool            `json:"is_success"`
	Result     json.RawMessage `json:"result"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ScheduledActionUpdateMessageType is the realtime message type used for
// live run updates (SCHEDULED_ACTION_UPDATE_MESSAGE_TYPE).
const ScheduledActionUpdateMessageType = "scheduled_action_update"

// ScheduledActionUpdateStarted mirrors the `started` variant of
// ScheduledActionUpdate (serde tag `type`, snake_case).
type ScheduledActionUpdateStarted struct {
	Type     string    `json:"type"` // "started"
	Owner    string    `json:"owner"`
	ActionID uuid.UUID `json:"action_id"`
	ChatID   string    `json:"chat_id"`
}

// ScheduledActionUpdateStopped mirrors the `stopped` variant.
type ScheduledActionUpdateStopped struct {
	Type      string    `json:"type"` // "stopped"
	Owner     string    `json:"owner"`
	ActionID  uuid.UUID `json:"action_id"`
	ChatID    string    `json:"chat_id"`
	IsSuccess bool      `json:"is_success"`
}

// AlreadyRunningError maps to HTTP 409 (mirrors domain::models::AlreadyRunningError).
type AlreadyRunningError struct {
	ActionID uuid.UUID
}

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("scheduled action %s is already running", e.ActionID)
}

// OwnerNotUserError maps to HTTP 400 (mirrors domain::models::OwnerNotUserError).
type OwnerNotUserError struct {
	Owner string
}

func (e *OwnerNotUserError) Error() string {
	return fmt.Sprintf("this path needs a user-owned scheduled action, but the owner is %q", e.Owner)
}

// NotFoundError maps to HTTP 404-ish service failures (the Rust service used a
// generic bail which surfaced as 500; we keep it explicit here).
type NotFoundError struct {
	ActionID uuid.UUID
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("scheduled action %s not found for user", e.ActionID)
}
