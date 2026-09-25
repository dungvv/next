// Provider + tool/MCP/agent interfaces. The full Rust port of ai_tools,
// mcp_select, and the agent loop is out of scope for this pass; everything
// hangs off these interfaces so the wiring is swappable.
package dcs

import (
	"context"
	"encoding/json"
)

// AIClient is the chat completion provider (Anthropic today, mirroring the
// Rust agent's model router which also supports OpenAI — TODO when the
// `openai/*` model ids are ported).
type AIClient interface {
	// StreamChat streams a completion. emit is invoked per content part
	// (text deltas arrive as they stream). The returned parts are the
	// accumulated assistant message for persistence.
	StreamChat(ctx context.Context, req *ChatRequest, emit func(part MessagePart) error) ([]MessagePart, *UsageInfo, error)
	// CompleteChat performs a non-streaming completion and returns the text.
	CompleteChat(ctx context.Context, req *ChatRequest) (string, *UsageInfo, error)
	// StructuredOutput performs a completion constrained to a JSON schema
	// (mirrors agent::structured_output::dynamic_structured_completion).
	StructuredOutput(ctx context.Context, req *ChatRequest, schema DynamicSchema) (json.RawMessage, error)
}

// UsageInfo mirrors ai_usage accounting data for a completion.
type UsageInfo struct {
	InputTokens  int
	OutputTokens int
}

// ChatRequest is the provider-agnostic completion request.
type ChatRequest struct {
	Model      string
	Messages   []ProviderMessage
	System     string
	MaxTokens  int
	Stream     bool
	Tools      []ToolSpec
	ToolChoice *ToolChoice
}

// ProviderMessage is one message in provider format (Anthropic wire shape).
type ProviderMessage struct {
	Role    string          `json:"role"`
	Content []ProviderBlock `json:"content"`
}

// ProviderBlock is one content block of a provider message.
type ProviderBlock struct {
	Type string `json:"type"` // text | image | tool_use | tool_result
	Text string `json:"text,omitempty"`
	// image
	Source *ImageSource `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ImageSource is an image for provider messages — "base64" (media_type+data)
// or "url" (url), matching Anthropic's image source variants.
type ImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ToolSpec mirrors the Anthropic tool definition used by ai_tools.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolChoice mirrors Anthropic's tool_choice field.
type ToolChoice struct {
	Type string `json:"type"` // auto | any | tool
	Name string `json:"name,omitempty"`
}

// DynamicSchema mirrors agent::structured_output::DynamicSchema.
type DynamicSchema struct {
	Schema      json.RawMessage `json:"schema"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
}

// UnmarshalJSON reads the DynamicSchema request field.
func (d *DynamicSchema) UnmarshalJSON(data []byte) error {
	var aux struct {
		Schema      json.RawMessage `json:"schema"`
		Name        string          `json:"name"`
		Description *string         `json:"description"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	d.Schema = aux.Schema
	d.Name = aux.Name
	if aux.Description != nil {
		d.Description = *aux.Description
	}
	return nil
}

// AttachmentResolver resolves entities attached to a chat message into
// provider-ready content parts (mirrors the sync-service attachment fetch +
// attachment::FormattedParts).
//
// TODO(attachments): port the sync service fetcher — the Rust implementation
// pulls document text/images per entity and caches resolved parts in
// resolved_message_content.
type AttachmentResolver interface {
	// Resolve returns content parts for the given entities, in order.
	Resolve(ctx context.Context, userID string, entities []Entity) ([]ResolvedPart, error)
}

// ResolvedPart is one attachment content unit.
type ResolvedPart struct {
	Kind      string // "text" | "image"
	Text      string
	MediaType string // image mime type when Kind == "image"
	Data      string // base64 payload when Kind == "image"
}

// ToolRunner executes AI tools for the /chats/{id}/tool/* endpoints and
// provides tool specs + prompt text for toolset=all.
//
// TODO(tools): implement over a port of ai_tools::tools_for(AiHost::Chat)
// plus the mcp_select connector set.
type ToolRunner interface {
	// Call executes a named tool with JSON args.
	Call(ctx context.Context, userID, name string, args json.RawMessage) (json.RawMessage, error)
	// Definitions returns the provider tool specs for the user's toolset
	// (static tools + MCP connectors).
	Definitions(ctx context.Context, userID string) ([]ToolSpec, error)
	// Prompt returns the complete system prompt for toolset=all — the base
	// prompt plus the tool documentation (mirrors all_tools.prompt built by
	// ai_tools::tools_for).
	Prompt(ctx context.Context) string
}

// MemoryProvider supplies the per-user memory block injected into the system
// prompt (mirrors memory_service::get_or_generate_memory).
//
// TODO(memory): port the memory service.
type MemoryProvider interface {
	Memory(ctx context.Context, userID string) (string, error)
}

// ChatNotifier emits the "chat response ready" notification after a stream
// completes (mirrors notify() in stream_and_save_message).
//
// TODO(notifications): port the notification ingress call.
type ChatNotifier interface {
	NotifyChatResponse(ctx context.Context, chatID, messageID, text, userID string)
}
