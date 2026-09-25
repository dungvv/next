// Docker Engine adapter for RuntimePort.
//
// Talks to the daemon over its HTTP API — /var/run/docker.sock by default —
// via the official client, replacing the Rust service's ECS task spawn and
// the dev-only `docker` CLI wrapper in crates/agent_harness/outbound/local.
//
// SECURITY: the docker socket is host-root-equivalent. Every spawned task
// gets the configured resource limits and only allowlisted env vars; the
// daemon itself must be reachable only from this process. See the package
// doc for the isolation TODO (gVisor / k8s Job adapter).
package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerConfig configures the Docker adapter.
type DockerConfig struct {
	// Host is the daemon address, e.g. "unix:///var/run/docker.sock".
	// Empty falls back to DOCKER_HOST, then the client default.
	Host string
	// Network is the container network spawned tasks join, so callers can
	// dial them by name (the Rust LOCAL_CONTAINER_NETWORK equivalent).
	Network string
	// Allowlist gates which env keys may reach a task.
	Allowlist EnvAllowlist
	// PullMissing pulls an image that is not already on the daemon.
	// Off by default: the Rust provider fails loudly on a missing image so
	// a typo'd tag does not become a silent multi-minute spawn.
	PullMissing bool
	// Secrets resolves `secret:NAME` env values at spawn time. Nil means no
	// secret indirection — values are taken literally.
	Secrets SecretPort
}

// DockerRuntime drives a Docker daemon.
type DockerRuntime struct {
	cli *client.Client
	cfg DockerConfig
}

// NewDockerRuntime connects to the daemon and verifies it answers.
func NewDockerRuntime(ctx context.Context, cfg DockerConfig) (*DockerRuntime, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if cfg.Host != "" {
		opts = append(opts, client.WithHost(cfg.Host))
	} else {
		opts = append(opts, client.FromEnv)
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	rt := &DockerRuntime{cli: cli, cfg: cfg}
	if _, err := cli.Ping(ctx); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("docker ping: %w", err)
	}
	return rt, nil
}

// Close releases the client.
func (d *DockerRuntime) Close() error { return d.cli.Close() }

// envList renders spec.Env through the allowlist and the secret resolver
// into KEY=VALUE form, sorted for deterministic container specs.
func (d *DockerRuntime) envList(ctx context.Context, env map[string]string) ([]string, error) {
	allowed, dropped := d.cfg.Allowlist.Filter(env)
	if len(dropped) > 0 {
		sort.Strings(dropped)
		slog.Warn("runtime: dropped env vars not in allowlist", "keys", dropped)
	}
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := allowed[k]
		if name, ok := strings.CutPrefix(v, "secret:"); ok && d.cfg.Secrets != nil {
			resolved, err := d.cfg.Secrets.Resolve(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("resolve env %s: %w", k, err)
			}
			v = resolved
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// Spawn implements RuntimePort.
func (d *DockerRuntime) Spawn(ctx context.Context, spec TaskSpec) (Task, error) {
	if _, err := d.cli.ImageInspect(ctx, spec.Image); err != nil {
		if !d.cfg.PullMissing {
			return Task{}, fmt.Errorf("image %q missing on daemon: %w", spec.Image, errors.Join(ErrUnavailable, err))
		}
		rc, err := d.cli.ImagePull(ctx, spec.Image, image.PullOptions{})
		if err != nil {
			return Task{}, fmt.Errorf("pull image %q: %w", spec.Image, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}

	// Deterministic name: a stale task for the same session is removed so a
	// re-spawn is not blocked by the name it already took.
	if spec.Name != "" {
		if stale, err := d.cli.ContainerInspect(ctx, spec.Name); err == nil {
			slog.Warn("runtime: removing stale task before respawn", "name", spec.Name, "id", stale.ID)
			_ = d.cli.ContainerRemove(ctx, stale.ID, container.RemoveOptions{Force: true})
		}
	}

	env, err := d.envList(ctx, spec.Env)
	if err != nil {
		return Task{}, err
	}

	labels := map[string]string{ManagedLabel: "true"}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(d.cfg.Network),
		Resources: container.Resources{
			NanoCPUs: spec.Limits.NanoCPUs,
			Memory:   spec.Limits.MemoryBytes,
		},
		// Sandboxes run model-authored code: drop every capability and
		// forbid privilege escalation. The agent image needs none.
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: false, // agents write to /workspace
	}
	createResp, err := d.cli.ContainerCreate(ctx, &container.Config{
		Image:  spec.Image,
		Env:    env,
		Labels: labels,
		// Mirrors the Rust provider: the image's CMD is an interactive shell
		// that exits without a TTY; the container must outlive its
		// entrypoint for exec/dial to have anything to enter.
		Cmd: firstNonEmpty(spec.Cmd, []string{"sleep", "infinity"}),
	}, hostCfg, &network.NetworkingConfig{}, nil, spec.Name)
	if err != nil {
		return Task{}, fmt.Errorf("create task %q: %w", spec.Name, errors.Join(ErrUnavailable, err))
	}
	if err := d.cli.ContainerStart(ctx, createResp.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(ctx, createResp.ID, container.RemoveOptions{Force: true})
		return Task{}, fmt.Errorf("start task %q: %w", spec.Name, errors.Join(ErrUnavailable, err))
	}
	return Task{ID: createResp.ID, Name: spec.Name, Labels: labels}, nil
}

// Start implements RuntimePort.
func (d *DockerRuntime) Start(ctx context.Context, taskID string) error {
	if err := d.cli.ContainerStart(ctx, taskID, container.StartOptions{}); err != nil {
		return mapErr(err)
	}
	return nil
}

// Stop implements RuntimePort.
func (d *DockerRuntime) Stop(ctx context.Context, taskID string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if err := d.cli.ContainerStop(ctx, taskID, container.StopOptions{Timeout: &secs}); err != nil {
		return mapErr(err)
	}
	return nil
}

// Remove implements RuntimePort.
func (d *DockerRuntime) Remove(ctx context.Context, taskID string) error {
	if err := d.cli.ContainerRemove(ctx, taskID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
		return mapErr(err)
	}
	return nil
}

// Status implements RuntimePort.
func (d *DockerRuntime) Status(ctx context.Context, taskID string) (Status, error) {
	info, err := d.cli.ContainerInspect(ctx, taskID)
	if err != nil {
		return Status{}, mapErr(err)
	}
	st := Status{State: TaskStateUnknown, ExitCode: -1}
	if info.State != nil {
		st.ExitCode = info.State.ExitCode
		switch {
		case info.State.Running:
			st.State = TaskStateRunning
		case info.State.Status == "created":
			st.State = TaskStateCreated
		case info.State.Status == "exited" || info.State.Status == "dead":
			st.State = TaskStateExited
		default:
			st.State = TaskState(info.State.Status)
		}
		st.StartedAt, _ = time.Parse(time.RFC3339Nano, info.State.StartedAt)
		st.FinishedAt, _ = time.Parse(time.RFC3339Nano, info.State.FinishedAt)
	}
	return st, nil
}

// Logs implements RuntimePort. The returned stream is demultiplexed: Docker
// frames stdout/stderr with 8-byte headers on non-TTY containers.
func (d *DockerRuntime) Logs(ctx context.Context, taskID string, opts LogOptions) (io.ReadCloser, error) {
	tail := "all"
	if opts.Tail > 0 {
		tail = fmt.Sprintf("%d", opts.Tail)
	}
	rc, err := d.cli.ContainerLogs(ctx, taskID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Follow:     opts.Follow,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = rc.Close()
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

// Exec implements RuntimePort: runs `bash -lc <cmd>` inside the task,
// matching the Rust local provider's login-shell exec.
func (d *DockerRuntime) Exec(ctx context.Context, taskID string, cmd []string, timeout time.Duration) (ExecResult, error) {
	if len(cmd) == 1 {
		cmd = []string{"bash", "-lc", cmd[0]}
	}
	createResp, err := d.cli.ContainerExecCreate(ctx, taskID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, mapErr(err)
	}

	waitCtx := ctx
	var cancel context.CancelFunc = func() {}
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	resp, err := d.cli.ContainerExecAttach(waitCtx, createResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, mapErr(err)
	}
	var buf bytes.Buffer
	_, copyErr := stdcopy.StdCopy(&buf, &buf, resp.Reader)
	resp.Close()
	if copyErr != nil {
		return ExecResult{ExitCode: -1, Output: buf.String()}, fmt.Errorf("read exec output: %w", copyErr)
	}
	if waitCtx.Err() != nil {
		return ExecResult{ExitCode: -1, Output: buf.String()},
			fmt.Errorf("exec timed out after %s: %w", timeout, ErrUnavailable)
	}
	ins, err := d.cli.ContainerExecInspect(ctx, createResp.ID)
	if err != nil {
		return ExecResult{ExitCode: -1, Output: buf.String()}, mapErr(err)
	}
	return ExecResult{ExitCode: ins.ExitCode, Output: buf.String()}, nil
}

// FindByLabel implements RuntimePort.
func (d *DockerRuntime) FindByLabel(ctx context.Context, key, value string) (*Task, error) {
	args := filters.NewArgs(filters.Arg("label", key+"="+value))
	list, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", errors.Join(ErrUnavailable, err))
	}
	if len(list) == 0 {
		return nil, nil
	}
	// Newest first by Created (unix seconds).
	sort.Slice(list, func(i, j int) bool { return list[i].Created > list[j].Created })
	s := list[0]
	return &Task{ID: s.ID, Name: strings.TrimPrefix(firstOr(s.Names, ""), "/"), Labels: s.Labels}, nil
}

// ListByLabel implements RuntimePort.
func (d *DockerRuntime) ListByLabel(ctx context.Context, key string) ([]Task, error) {
	args := filters.NewArgs(filters.Arg("label", key))
	list, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", errors.Join(ErrUnavailable, err))
	}
	out := make([]Task, 0, len(list))
	for _, s := range list {
		out = append(out, Task{ID: s.ID, Name: strings.TrimPrefix(firstOr(s.Names, ""), "/"), Labels: s.Labels})
	}
	return out, nil
}

// ShutdownAll implements RuntimePort: removes every managed task so a stack
// stop leaves no `macro-agent-*` containers behind.
func (d *DockerRuntime) ShutdownAll(ctx context.Context) int {
	tasks, err := d.ListByLabel(ctx, ManagedLabel)
	if err != nil {
		slog.Error("runtime: failed to list managed tasks for shutdown", "err", err)
		return 1
	}
	failures := 0
	for _, t := range tasks {
		if err := d.Remove(ctx, t.ID); err != nil {
			slog.Error("runtime: failed to remove task on shutdown", "task", t.Name, "err", err)
			failures++
		}
	}
	return failures
}

func mapErr(err error) error {
	if client.IsErrNotFound(err) {
		return errors.Join(ErrNotFound, err)
	}
	return errors.Join(ErrUnavailable, err)
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}
