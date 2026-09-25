package scheduled

import (
	"context"

	"github.com/google/uuid"
)

// Executor runs one action (claims it, kicks the task, records the run).
type Executor interface {
	ExecuteAction(ctx context.Context, a ScheduledAction) (InProgressExecution, error)
}

// Service mirrors domain::service::ScheduledActionServiceImpl. The Rust
// version forwarded create/update/delete events to an in-memory dispatcher;
// the Go port's dispatcher polls Postgres, so those events are redundant and
// intentionally absent.
type Service struct {
	repo     *Repo
	executor Executor
}

// NewService builds the service.
func NewService(repo *Repo, executor Executor) *Service {
	return &Service{repo: repo, executor: executor}
}

func isOwnedBy(a *ScheduledAction, caller string) bool { return a.Owner == caller }

// CreateAction persists a new action.
func (s *Service) CreateAction(ctx context.Context, a ScheduledAction) (ScheduledAction, error) {
	return s.repo.CreateAction(ctx, a)
}

// GetActions lists the caller's actions.
func (s *Service) GetActions(ctx context.Context, userID string) ([]ScheduledAction, error) {
	return s.repo.GetActions(ctx, userID)
}

// findOwned returns the action iff it exists and belongs to the caller.
func (s *Service) findOwned(ctx context.Context, id uuid.UUID, caller string) (*ScheduledAction, error) {
	actions, err := s.repo.GetActions(ctx, caller)
	if err != nil {
		return nil, err
	}
	for i := range actions {
		if actions[i].ID == id && isOwnedBy(&actions[i], caller) {
			return &actions[i], nil
		}
	}
	return nil, &NotFoundError{ActionID: id}
}

// UpdateAction validates ownership then rewrites the action.
func (s *Service) UpdateAction(ctx context.Context, a ScheduledAction, caller string) (ScheduledAction, error) {
	if _, err := s.findOwned(ctx, a.ID, caller); err != nil {
		return ScheduledAction{}, err
	}
	return s.repo.UpdateAction(ctx, a)
}

// DeleteAction validates ownership then removes the action.
func (s *Service) DeleteAction(ctx context.Context, id uuid.UUID, caller string) error {
	if _, err := s.findOwned(ctx, id, caller); err != nil {
		return err
	}
	return s.repo.DeleteAction(ctx, id)
}

// ExecuteActionNow runs the action immediately (claim still applies).
func (s *Service) ExecuteActionNow(ctx context.Context, id uuid.UUID, caller string) (InProgressExecution, error) {
	action, err := s.findOwned(ctx, id, caller)
	if err != nil {
		return InProgressExecution{}, err
	}
	return s.executor.ExecuteAction(ctx, *action)
}

// GetExecutionRecords lists run history for a caller-owned action.
func (s *Service) GetExecutionRecords(ctx context.Context, id uuid.UUID, caller string) ([]ActionExecutionRecord, error) {
	if _, err := s.findOwned(ctx, id, caller); err != nil {
		return nil, err
	}
	return s.repo.GetExecutionRecords(ctx, id)
}
