// Port of agent::convert — ChatMessage list → provider message list.
// Anthropic's wire format accepts the same turn structure rig produces:
// user text blocks, assistant text/tool_use blocks, and tool_result user
// turns.
package dcs

import (
	"encoding/json"
	"strings"
)

// toProviderMessages mirrors agent::to_rig_messages: converts stored chat
// messages into provider turns, reconstructing assistant/tool_result
// alternation from the flat part list.
func toProviderMessages(messages []storedMessage, attachmentParts []ResolvedPart) []ProviderMessage {
	var out []ProviderMessage
	for _, m := range messages {
		switch m.role {
		case RoleSystem:
			continue
		case RoleUser:
			out = append(out, convertUserMessage(m))
		case RoleAssistant:
			out = append(out, convertAssistantMessage(m)...)
		}
	}
	return out
}

// storedMessage is the internal message shape used to build requests.
type storedMessage struct {
	role    string
	content MessageContent
	// resolved attachment parts already fetched for this message
	attachmentParts []ResolvedPart
}

// convertUserMessage mirrors convert_user: text block first, then resolved
// attachment content blocks.
func convertUserMessage(m storedMessage) ProviderMessage {
	var content []ProviderBlock
	if text := m.content.MessageText(); text != "" {
		content = append(content, ProviderBlock{Type: "text", Text: text})
	}
	for _, part := range m.attachmentParts {
		switch part.Kind {
		case "image":
			content = append(content, ProviderBlock{
				Type:   "image",
				Source: &ImageSource{Type: "base64", MediaType: part.MediaType, Data: part.Data},
			})
		case "image_url":
			content = append(content, ProviderBlock{
				Type:   "image",
				Source: &ImageSource{Type: "url", URL: part.Text},
			})
		default:
			content = append(content, ProviderBlock{Type: "text", Text: part.Text})
		}
	}
	if len(content) == 0 {
		content = append(content, ProviderBlock{Type: "text", Text: ""})
	}
	return ProviderMessage{Role: RoleUser, Content: content}
}

// convertAssistantMessage mirrors convert_assistant: text parts merge into
// assistant turns; tool calls and their results re-form the
// assistant(tool_use) → user(tool_result) alternation providers expect.
func convertAssistantMessage(m storedMessage) []ProviderMessage {
	if text, ok := m.content.Text(); ok {
		return []ProviderMessage{{
			Role:    RoleAssistant,
			Content: []ProviderBlock{{Type: "text", Text: text}},
		}}
	}
	parts, ok := m.content.Parts()
	if !ok {
		return nil
	}

	var out []ProviderMessage
	var assistantBlocks []ProviderBlock
	var toolResults []ProviderBlock
	sawToolCall := false

	flush := func() {
		if len(assistantBlocks) > 0 {
			out = append(out, ProviderMessage{Role: RoleAssistant, Content: assistantBlocks})
			assistantBlocks = nil
		}
		if len(toolResults) > 0 {
			out = append(out, ProviderMessage{Role: RoleUser, Content: toolResults})
			toolResults = nil
		}
	}

	for _, part := range parts {
		switch part.Type {
		case PartText:
			if sawToolCall {
				flush()
				sawToolCall = false
			}
			if len(assistantBlocks) > 0 && assistantBlocks[len(assistantBlocks)-1].Type == "text" {
				assistantBlocks[len(assistantBlocks)-1].Text += part.Text
			} else {
				assistantBlocks = append(assistantBlocks, ProviderBlock{Type: "text", Text: part.Text})
			}
		case PartToolCall, PartMCPToolCall:
			sawToolCall = true
			input := part.JSON
			// Anthropic requires tool_use.input to be an object; older
			// persisted messages may carry a non-object value.
			if !isJSONObject(input) {
				input = json.RawMessage(`{}`)
			}
			assistantBlocks = append(assistantBlocks, ProviderBlock{
				Type:  "tool_use",
				ID:    replayCallID(part.ID),
				Name:  part.Name,
				Input: input,
			})
		case PartToolCallResponseJSON:
			toolResults = append(toolResults, toolResultBlock(part.ID, string(part.JSON), false))
		case PartToolCallErr:
			toolResults = append(toolResults, toolResultBlock(part.ID, part.Description, false))
		case PartThinking:
			// thinking parts are not replayed
		}
	}
	flush()
	return out
}

func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil
}

func toolResultBlock(id, content string, isErr bool) ProviderBlock {
	// Anthropic tool_result content can be a plain string.
	raw, _ := json.Marshal(content)
	return ProviderBlock{
		Type:      "tool_result",
		ToolUseID: replayCallID(id),
		Content:   raw,
		IsError:   isErr,
	}
}

// replayCallID mirrors the Rust replay_call_id: the persisted id is replayed
// to the provider with trailing separators trimmed (a call and its result
// share the same id, so pairing survives). Anthropic requires only that
// tool_use.id matches tool_result.tool_use_id.
func replayCallID(id string) string {
	return strings.TrimRight(id, "_-")
}

// mergeConsecutiveParts mirrors agent::convert::merge_consecutive_parts.
func mergeConsecutiveParts(parts []MessagePart) []MessagePart {
	out := make([]MessagePart, 0, len(parts))
	for _, part := range parts {
		if len(out) > 0 {
			last := &out[len(out)-1]
			if last.Type == PartText && part.Type == PartText {
				last.Text += part.Text
				continue
			}
			if last.Type == PartThinking && part.Type == PartThinking {
				last.Thinking += part.Thinking
				continue
			}
		}
		out = append(out, part)
	}
	return out
}

// resolvePendingToolCalls mirrors the Rust function of the same name: on
// cancellation, every unanswered tool call gets a synthetic "cancelled"
// toolCallErr so the persisted message stays well-formed.
func resolvePendingToolCalls(parts []MessagePart) []MessagePart {
	pending := map[string]bool{}
	for _, p := range parts {
		switch p.Type {
		case PartToolCall, PartMCPToolCall:
			pending[p.ID] = true
		case PartToolCallResponseJSON, PartToolCallErr:
			delete(pending, p.ID)
		}
	}
	if len(pending) == 0 {
		return parts
	}
	out := make([]MessagePart, 0, len(parts)+len(pending))
	for _, p := range parts {
		out = append(out, p)
		if (p.Type == PartToolCall || p.Type == PartMCPToolCall) && pending[p.ID] {
			out = append(out, MessagePart{
				Type:        PartToolCallErr,
				ID:          p.ID,
				Name:        p.Name,
				Description: "cancelled",
			})
		}
	}
	return out
}
