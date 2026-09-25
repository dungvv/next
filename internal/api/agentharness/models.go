package agentharness

import (
	"encoding/json"
	"fmt"
	"time"

	runtime "github.com/macro-inc/macro/pkg/runtime"
)

// SandboxSize is the named compute tier of a managed sandbox — the port of
// crates/agent_harness/sandbox_sizes.json. The DB CHECK constraint admits
// exactly these three values.
type SandboxSize string

const (
	SandboxSmall   SandboxSize = "small"
	SandboxDefault SandboxSize = "default"
	SandboxLarge   SandboxSize = "large"
)

// valid reports whether the tier is one the schema admits.
func (s SandboxSize) valid() bool {
	switch s {
	case SandboxSmall, SandboxDefault, SandboxLarge:
		return true
	}
	return false
}

// limits maps the tier onto runtime resource bounds. DiskBytes is recorded
// for adapter parity but not enforced by Docker today.
func (s SandboxSize) limits() runtime.ResourceLimits {
	switch s {
	case SandboxSmall:
		return runtime.ResourceLimits{NanoCPUs: 2_000_000_000, MemoryBytes: 4 << 30, DiskBytes: 24 << 30}
	case SandboxLarge:
		return runtime.ResourceLimits{NanoCPUs: 8_000_000_000, MemoryBytes: 16 << 30, DiskBytes: 128 << 30}
	default:
		return runtime.ResourceLimits{NanoCPUs: 4_000_000_000, MemoryBytes: 8 << 30, DiskBytes: 96 << 30}
	}
}

// Session is one agent_session row plus its external-provider link, when one
// exists. egress_token_hash is loaded but never serialized.
type Session struct {
	ID                   string
	Name                 string
	OwnerID              string
	ThreadID             *string
	ThreadParentType     string // "channel" | "document" | ""
	ThreadParentID       *string
	OriginatingMessageID *string
	BotID                string
	Model                string
	Harness              string
	RepoURL              *string
	RepoBranch           *string
	WorkingBranch        *string
	PullRequestURL       *string
	Workspace            string
	SandboxSize          SandboxSize
	Instructions         *string
	Status               string // no_messages | event | disconnected
	StatusEventName      *string
	TurnState            *string
	ACPSessionID         *string
	EgressTokenHash      *string
	MCPScope             string
	MCPServers           []byte // jsonb
	External             *ExternalLink
	CreatedAt            time.Time
	ModifiedAt           time.Time
}

// ExternalLink is the external_agent_session row joining a session to a
// provider-side agent (cursor/codex/claude).
type ExternalLink struct {
	Provider     string
	ExternalID   string
	ExternalName *string
	ExternalURL  *string
}

// SessionLogEntry is one agent_session_log row.
type SessionLogEntry struct {
	ID        string
	SessionID string
	UserID    *string
	Direction string // to_server | to_runtime
	Content   []byte // jsonb
	CreatedAt time.Time
}

// ---- Wire DTOs (camelCase, matching the Rust axum router) -----------------

// CreateSessionRequest mirrors CreateAgentSessionRequest. workspace present
// asks for an external session; absent asks for a managed one.
type CreateSessionRequest struct {
	BotID        *string              `json:"botId"`
	Workspace    *string              `json:"workspace"`
	Prompt       *string              `json:"prompt"`
	RepoURL      *string              `json:"repoUrl"`
	RepoBranch   *string              `json:"repoBranch"`
	Owner        *string              `json:"owner"`
	Thread       *CreateSessionThread `json:"thread"`
	Instructions *string              `json:"instructions"`
}

// CreateSessionThread mirrors CreateSessionThread.
type CreateSessionThread struct {
	ParentType string  `json:"parentType"` // "channel" | "document"; empty + channelId = legacy
	ParentID   *string `json:"parentId"`
	ChannelID  *string `json:"channelId"`
	ThreadID   *string `json:"threadId"`
	MessageID  string  `json:"messageId"`
	Content    string  `json:"content"`
}

// SessionResponse mirrors AgentSessionResponse.
type SessionResponse struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	OwnerID              string            `json:"ownerId"`
	CanEdit              bool              `json:"canEdit"`
	ThreadID             *string           `json:"threadId"`
	ThreadParent         *ThreadParentRef  `json:"threadParent"`
	ThreadChannelID      *string           `json:"threadChannelId"`
	OriginatingMessageID *string           `json:"originatingMessageId"`
	BotID                string            `json:"botId"`
	Model                string            `json:"model"`
	Harness              string            `json:"harness"`
	RepoURL              *string           `json:"repoUrl,omitempty"`
	PullRequestURL       *string           `json:"pullRequestUrl"`
	Workspace            string            `json:"workspace"`
	SandboxSize          SandboxSize       `json:"sandboxSize"`
	Instructions         *string           `json:"instructions,omitempty"`
	ACPSessionID         *string           `json:"acpSessionId"`
	Status               SessionStatusDTO  `json:"status"`
	External             *ExternalResponse `json:"external,omitempty"`
	CreatedAt            time.Time         `json:"createdAt"`
	ModifiedAt           time.Time         `json:"modifiedAt"`
}

// ThreadParentRef mirrors messages::MessageParent on the wire.
type ThreadParentRef struct {
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
}

// SessionStatusDTO mirrors SessionStatusDto.
type SessionStatusDTO struct {
	Status    string  `json:"status"`              // "no_messages" | "event" | "disconnected"
	EventName *string `json:"eventName,omitempty"` // set when status == "event"
}

// ExternalResponse mirrors ExternalSessionResponse.
type ExternalResponse struct {
	Provider string  `json:"provider"`
	Name     *string `json:"name,omitempty"`
	URL      *string `json:"url,omitempty"`
}

// CreateSessionResponse mirrors CreateAgentSessionResponse.
type CreateSessionResponse struct {
	Session SessionResponse `json:"session"`
}

// ThreadSessionExistsResponse is the 409 body when a thread already routes
// to one of the bot's sessions.
type ThreadSessionExistsResponse struct {
	Message   string  `json:"message"`
	SessionID *string `json:"sessionId,omitempty"`
}

// RenameRequest mirrors RenameAgentSessionRequest.
type RenameRequest struct {
	Name string `json:"name"`
}

// PreviewRequest mirrors PreviewAgentSessionsRequest.
type PreviewRequest struct {
	SessionIDs []string `json:"sessionIds"`
}

// PreviewResponse mirrors the preview handler's body: one entry per
// requested session the caller may see.
type PreviewResponse struct {
	Sessions []SessionResponse `json:"sessions"`
}

// LogResponse wraps the session's folded log. The Rust endpoint serves the
// folded AgentSessionLog; this port serves the raw chronological rows and
// leaves protocol folding to a follow-up (TODO(log-fold)).
type LogResponse struct {
	Entries []SessionLogEntry `json:"entries"`
}

// ControlRequest mirrors ControlRequest: a wrapper around the action so the
// caller-supplied action id has somewhere to live. The Rust shape flattens
// the action's fields under "type"; UnmarshalJSON peels actionId off and
// keeps the rest verbatim for the runtime protocol to validate.
type ControlRequest struct {
	ActionID *string        `json:"-"`
	Action   map[string]any `json:"-"`
}

// UnmarshalJSON implements the Rust `#[serde(flatten)]` shape: every field
// except "actionId" is the action itself.
func (c *ControlRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if id, ok := raw["actionId"]; ok {
		s, ok := id.(string)
		if !ok {
			return fmt.Errorf("actionId must be a string")
		}
		c.ActionID = &s
		delete(raw, "actionId")
	}
	c.Action = raw
	return nil
}

// ControlResponse mirrors ControlResponse.
type ControlResponse struct {
	ActionID string `json:"actionId"`
	Status   string `json:"status"` // "sent" | "queued"
}

// QueueResponse mirrors AgentSessionQueueResponse.
type QueueResponse struct {
	Entries []QueuedAction `json:"entries"`
}

// QueuedAction mirrors QueuedActionDto: an action waiting for its turn.
type QueuedAction struct {
	ActionID string         `json:"actionId"`
	UserID   string         `json:"userId"`
	Action   map[string]any `json:"action"`
	QueuedAt time.Time      `json:"queuedAt"`
}

// EditQueuedActionRequest mirrors EditQueuedActionRequest.
type EditQueuedActionRequest struct {
	Prompt string `json:"prompt"`
}

// SandboxSizeRequest is the PUT body for user/session sandbox size.
type SandboxSizeRequest struct {
	Size SandboxSize `json:"sandboxSize"`
}

// SandboxSizeResponse is the GET body for the caller-default sandbox size.
type SandboxSizeResponse struct {
	Size SandboxSize `json:"sandboxSize"`
}
