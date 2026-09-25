// Package runtime ports the agent-sandbox container lifecycle off ECS
// (services/agent_harness_service + crates/agent_harness/outbound) onto a
// RuntimePort interface with a Docker Engine adapter for self-hosting.
//
// SECURITY: the Docker adapter drives the daemon over /var/run/docker.sock.
// That socket is root-equivalent on the host — the API process must run in a
// deployment where that is acceptable (dedicated node / VM), and agent
// containers get resource limits plus an env allowlist, but share the host
// kernel. TODO(isolation): add a gVisor (runsc) runtime option or a
// Kubernetes Job adapter for stronger sandbox isolation before exposing
// managed agents to untrusted tenants.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// SessionLabel is the container label carrying the owning agent session id —
// the port of crates/agent_harness provision::SESSION_LABEL. Resume and
// teardown look containers up by it.
const SessionLabel = "macro.agent_session_id"

// ManagedLabel marks every container this deployment created, so ShutdownAll
// and orphan sweeps never touch unrelated workloads on the daemon.
const ManagedLabel = "macro.managed"

// ErrNotFound is returned when a task does not exist.
var ErrNotFound = errors.New("runtime: task not found")

// ErrUnavailable is returned when the runtime backend cannot be reached or a
// task is not in a state that allows the operation.
var ErrUnavailable = errors.New("runtime: unavailable")

// ResourceLimits are the compute bounds applied to a spawned task. Zero
// fields leave the daemon default in place. Named tiers map onto these in
// the agentharness module (Rust: crates/agent_harness/sandbox_sizes.json).
type ResourceLimits struct {
	// NanoCPUs is CPU quota in units of 1e-9 CPUs (2 CPUs = 2_000_000_000).
	NanoCPUs int64
	// MemoryBytes is the hard memory cap.
	MemoryBytes int64
	// DiskBytes is reserved for image-layers quota; not enforced by the
	// Docker adapter today (TODO: needs daemon storage-opts support).
	DiskBytes int64
}

// TaskSpec describes one task to spawn.
type TaskSpec struct {
	// Name is the deterministic task name (e.g. "macro-agent-<session>").
	// A stale task with the same name is replaced.
	Name string
	// Image is the image reference to run.
	Image string
	// Cmd overrides the image entrypoint. Empty runs the image's CMD.
	Cmd []string
	// Env is the environment handed to the task. Only keys in the
	// adapter's allowlist are passed through; others are dropped.
	Env map[string]string
	// Labels tag the task for lookup (must include SessionLabel).
	Labels map[string]string
	// Network is the container network the task joins so callers can dial
	// it by name. Empty uses the daemon default.
	Network string
	// Limits bounds the task's compute.
	Limits ResourceLimits
}

// Task is one spawned unit of work.
type Task struct {
	// ID is the runtime-native identifier (container id).
	ID string
	// Name is the spec name the task was created under.
	Name string
	// Labels are the labels the task carries.
	Labels map[string]string
}

// TaskState mirrors Docker container states.
type TaskState string

const (
	TaskStateRunning TaskState = "running"
	TaskStateStopped TaskState = "stopped"
	TaskStateExited  TaskState = "exited"
	TaskStateCreated TaskState = "created"
	TaskStateUnknown TaskState = "unknown"
)

// Status reports where a task is in its lifecycle.
type Status struct {
	State      TaskState
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

// LogOptions control a Logs read.
type LogOptions struct {
	// Tail limits the returned lines (0 = all).
	Tail int
	// Follow keeps the stream open for new output.
	Follow bool
}

// ExecResult is the outcome of running a command inside a task.
type ExecResult struct {
	ExitCode int
	// Output is combined stdout+stderr, matching the Rust docker CLI
	// adapter's combined-output behavior.
	Output string
}

// RuntimePort is the spawn/stop/status/logs surface a task runtime provides.
// It replaces the Rust ContainerManager port (crates/agent_harness
// domain/ports.rs) and the ECS/Daytona adapters behind it.
type RuntimePort interface {
	// Spawn creates and starts a task. A stale task carrying the same name
	// is removed first, so re-spawning a session is safe.
	Spawn(ctx context.Context, spec TaskSpec) (Task, error)
	// Start resumes a stopped-but-existing task.
	Start(ctx context.Context, taskID string) error
	// Stop halts a task without removing it.
	Stop(ctx context.Context, taskID string, timeout time.Duration) error
	// Remove destroys a task, running or not.
	Remove(ctx context.Context, taskID string) error
	// Status reports a task's lifecycle state.
	Status(ctx context.Context, taskID string) (Status, error)
	// Logs returns the task's combined output stream. The caller closes it.
	Logs(ctx context.Context, taskID string, opts LogOptions) (io.ReadCloser, error)
	// Exec runs one command inside a running task and returns its exit code
	// and combined output. Timeout cancels the wait, not the command.
	Exec(ctx context.Context, taskID string, cmd []string, timeout time.Duration) (ExecResult, error)
	// FindByLabel returns the newest task carrying key=value, or nil.
	FindByLabel(ctx context.Context, key, value string) (*Task, error)
	// ListByLabel returns every task carrying key, running or not.
	ListByLabel(ctx context.Context, key string) ([]Task, error)
	// ShutdownAll removes every managed task (ManagedLabel). Returns the
	// number of tasks that could not be removed.
	ShutdownAll(ctx context.Context) int
	// Close releases the runtime client.
	Close() error
}
