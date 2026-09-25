// Minimal Anthropic Messages API client (replaces the rig-based client in
// crates/agent). Streams SSE from POST /v1/messages.
package dcs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicClient implements AIClient against the Anthropic Messages API.
type AnthropicClient struct {
	apiKey    string
	baseURL   string
	version   string
	maxTokens int
	http      *http.Client
}

// NewAnthropicClient builds the default provider client.
func NewAnthropicClient(apiKey, baseURL, version string, maxTokens int) *AnthropicClient {
	return &AnthropicClient{
		apiKey:    apiKey,
		baseURL:   strings.TrimRight(baseURL, "/"),
		version:   version,
		maxTokens: maxTokens,
		http:      &http.Client{Timeout: 0}, // streams are ctx-bound
	}
}

// modelName strips the "anthropic/" provider prefix used in chat model ids.
func modelName(model string) (string, error) {
	if strings.HasPrefix(model, "anthropic/") {
		return strings.TrimPrefix(model, "anthropic/"), nil
	}
	if strings.Contains(model, "/") {
		return "", fmt.Errorf("model provider of %q not supported yet (anthropic/* only)", model)
	}
	return model, nil
}

// anthropicRequest is the POST /v1/messages body.
type anthropicRequest struct {
	Model      string            `json:"model"`
	Messages   []ProviderMessage `json:"messages"`
	MaxTokens  int               `json:"max_tokens"`
	System     string            `json:"system,omitempty"`
	Stream     bool              `json:"stream"`
	Tools      []ToolSpec        `json:"tools,omitempty"`
	ToolChoice *ToolChoice       `json:"tool_choice,omitempty"`
}

func (c *AnthropicClient) buildBody(req *ChatRequest, stream bool) ([]byte, error) {
	model, err := modelName(req.Model)
	if err != nil {
		return nil, err
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.maxTokens
	}
	return json.Marshal(anthropicRequest{
		Model:      model,
		Messages:   req.Messages,
		MaxTokens:  maxTokens,
		System:     req.System,
		Stream:     stream,
		Tools:      req.Tools,
		ToolChoice: req.ToolChoice,
	})
}

func (c *AnthropicClient) do(ctx context.Context, body []byte) (*http.Response, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", c.version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &ProviderError{Status: resp.StatusCode, Body: string(b)}
	}
	return resp, nil
}

// ProviderError mirrors an HTTP failure from the provider.
type ProviderError struct {
	Status int
	Body   string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("anthropic: status %d: %s", e.Status, e.Body)
}

// isContextOverflow mirrors the Rust classifier: Anthropic returns "prompt is
// too long" for context-window overflows.
func (e *ProviderError) isContextOverflow() bool {
	body := strings.ToLower(e.Body)
	return strings.Contains(body, "prompt is too long") ||
		strings.Contains(body, "context length") ||
		e.Status == http.StatusRequestEntityTooLarge
}

// ---------------------------------------------------------------------------
// SSE stream
// ---------------------------------------------------------------------------

// streamEvent is one Anthropic SSE `data:` payload.
type streamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Message      json.RawMessage `json:"message"`
	Usage        *usageObj       `json:"usage"`
	Error        *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type usageObj struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type contentBlock struct {
	Type  string          `json:"type"` // text | thinking | tool_use | ...
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type contentDelta struct {
	Type        string `json:"type"` // text_delta | thinking_delta | input_json_delta | signature_delta
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
}

// streamAccumulator gathers provider deltas into persisted MessageParts and
// forwards each emitted part to the client (mirrors agent::StreamAccumulator).
type streamAccumulator struct {
	parts []MessagePart
	// openBlocks maps a content-block index to its kind: "text", "thinking",
	// or "tool_use".
	openBlocks map[int]string
	// toolMeta/toolJSON track in-progress tool_use blocks.
	toolMeta map[int]contentBlock
	toolJSON map[int]*strings.Builder
	emit     func(part MessagePart) error
}

func newStreamAccumulator(emit func(part MessagePart) error) *streamAccumulator {
	return &streamAccumulator{
		openBlocks: map[int]string{},
		toolMeta:   map[int]contentBlock{},
		toolJSON:   map[int]*strings.Builder{},
		emit:       emit,
	}
}

// push handles one SSE data payload.
func (a *streamAccumulator) push(ev streamEvent) error {
	switch ev.Type {
	case "message_start":
		return nil
	case "content_block_start":
		var blk contentBlock
		if err := json.Unmarshal(ev.ContentBlock, &blk); err != nil {
			return nil // unknown block shape — skip
		}
		switch blk.Type {
		case "text":
			a.openBlocks[ev.Index] = "text"
		case "thinking":
			a.openBlocks[ev.Index] = "thinking"
		case "tool_use":
			a.openBlocks[ev.Index] = "tool_use"
			a.toolMeta[ev.Index] = blk
			a.toolJSON[ev.Index] = &strings.Builder{}
			if len(blk.Input) > 0 && string(blk.Input) != "{}" {
				a.toolJSON[ev.Index].Write(blk.Input)
			}
		default:
			a.openBlocks[ev.Index] = blk.Type
		}
		return nil
	case "content_block_delta":
		var d contentDelta
		if err := json.Unmarshal(ev.Delta, &d); err != nil {
			return nil
		}
		switch d.Type {
		case "text_delta":
			if d.Text == "" {
				return nil
			}
			part := TextPart(d.Text)
			a.parts = append(a.parts, part)
			return a.emit(part)
		case "thinking_delta":
			if d.Thinking == "" {
				return nil
			}
			part := MessagePart{Type: PartThinking, Thinking: d.Thinking}
			a.parts = append(a.parts, part)
			return a.emit(part)
		case "input_json_delta":
			if b := a.toolJSON[ev.Index]; b != nil {
				b.WriteString(d.PartialJSON)
			}
			return nil
		default:
			return nil // signature_delta, citations_delta, …
		}
	case "content_block_stop":
		if a.openBlocks[ev.Index] == "tool_use" {
			blk := a.toolMeta[ev.Index]
			input := blk.Input
			if b := a.toolJSON[ev.Index]; b != nil && b.Len() > 0 {
				raw := json.RawMessage(b.String())
				if json.Valid(raw) {
					input = raw
				}
			}
			if !json.Valid(input) || len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			part := MessagePart{Type: PartToolCall, ID: blk.ID, Name: blk.Name, JSON: input}
			a.parts = append(a.parts, part)
			delete(a.openBlocks, ev.Index)
			delete(a.toolMeta, ev.Index)
			delete(a.toolJSON, ev.Index)
			return a.emit(part)
		}
		delete(a.openBlocks, ev.Index)
		return nil
	case "message_delta", "message_stop", "ping":
		return nil
	case "error":
		if ev.Error != nil {
			return &ProviderError{Status: 0, Body: ev.Error.Message}
		}
		return &ProviderError{Status: 0, Body: "unknown stream error"}
	default:
		return nil
	}
}

// stream parses the Anthropic SSE body, forwarding emitted parts.
func (c *AnthropicClient) stream(ctx context.Context, req *ChatRequest, emit func(part MessagePart) error) ([]MessagePart, *UsageInfo, error) {
	body, err := c.buildBody(req, true)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	acc := newStreamAccumulator(emit)
	usage := &UsageInfo{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // keep-alive / unknown payloads
		}
		// message_delta carries cumulative usage.
		if ev.Usage != nil {
			usage.OutputTokens = ev.Usage.OutputTokens
			if ev.Usage.InputTokens > 0 {
				usage.InputTokens = ev.Usage.InputTokens
			}
		}
		if ev.Type == "message_start" {
			var msg struct {
				Usage *usageObj `json:"usage"`
			}
			if err := json.Unmarshal(ev.Message, &msg); err == nil && msg.Usage != nil {
				usage.InputTokens = msg.Usage.InputTokens
			}
		}
		if err := acc.push(ev); err != nil {
			return acc.parts, usage, err
		}
		if err := ctx.Err(); err != nil {
			return acc.parts, usage, err
		}
	}
	if err := scanner.Err(); err != nil {
		return acc.parts, usage, err
	}
	// Flush any tool_use block left open by a truncated stream.
	for idx, kind := range acc.openBlocks {
		if kind == "tool_use" {
			blk := acc.toolMeta[idx]
			input := json.RawMessage(`{}`)
			if b := acc.toolJSON[idx]; b != nil && b.Len() > 0 {
				if raw := json.RawMessage(b.String()); json.Valid(raw) {
					input = raw
				}
			}
			part := MessagePart{Type: PartToolCall, ID: blk.ID, Name: blk.Name, JSON: input}
			acc.parts = append(acc.parts, part)
			if err := acc.emit(part); err != nil {
				return acc.parts, usage, err
			}
		}
	}
	return acc.parts, usage, nil
}

// StreamChat implements AIClient.
func (c *AnthropicClient) StreamChat(ctx context.Context, req *ChatRequest, emit func(part MessagePart) error) ([]MessagePart, *UsageInfo, error) {
	return c.stream(ctx, req, emit)
}

// messageResponse is the non-streaming /v1/messages response.
type messageResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage usageObj `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CompleteChat implements AIClient (non-streaming completion → text).
func (c *AnthropicClient) CompleteChat(ctx context.Context, req *ChatRequest) (string, *UsageInfo, error) {
	body, err := c.buildBody(req, false)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	var out messageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	if out.Error != nil {
		return "", nil, &ProviderError{Status: 0, Body: out.Error.Message}
	}
	var text strings.Builder
	for _, blk := range out.Content {
		if blk.Type == "text" {
			text.WriteString(blk.Text)
		}
	}
	return text.String(), &UsageInfo{
		InputTokens:  out.Usage.InputTokens,
		OutputTokens: out.Usage.OutputTokens,
	}, nil
}

// StructuredOutput implements AIClient — mirrors
// agent::structured_output::dynamic_structured_completion: a plain completion
// with a JSON-only instruction, then strip markdown fences and parse.
func (c *AnthropicClient) StructuredOutput(ctx context.Context, req *ChatRequest, schema DynamicSchema) (json.RawMessage, error) {
	var sb strings.Builder
	sb.WriteString(req.System)
	sb.WriteString("\n\nYou MUST respond with ONLY a valid JSON object matching this schema.\nSchema name: ")
	sb.WriteString(schema.Name)
	sb.WriteString("\n")
	if schema.Description != "" {
		sb.WriteString(schema.Description)
		sb.WriteString("\n")
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema.Schema), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	sb.WriteString("Schema:\n```json\n")
	sb.Write(pretty)
	sb.WriteString("\n```\nRespond with ONLY the raw JSON object. No markdown fences, no explanation.")

	text, _, err := c.CompleteChat(ctx, &ChatRequest{
		Model:     req.Model,
		Messages:  req.Messages,
		System:    sb.String(),
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(trimmed, "```"); ok {
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimSuffix(rest, "```")
		trimmed = strings.TrimSpace(rest)
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("structured output was not valid JSON")
	}
	return json.RawMessage(trimmed), nil
}
