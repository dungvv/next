// Tool-call endpoints — Go port of the crates/chat tool handlers
// (update_tool_call, update_tool_response, call_tool, reject_tool_call).
// All require owner access and manipulate the persisted AssistantMessageParts
// of the message that carries the tool call.
package dcs

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// toolCallParts mirrors get_tool_call_parts: the message content must be an
// assistant-parts array containing a ToolCall/McpToolCall with toolCallID.
func (s *Service) toolCallParts(ctx context.Context, chatID, messageID, toolCallID string) ([]MessagePart, error) {
	content, err := s.repo.getMessageContent(ctx, chatID, messageID)
	if err != nil {
		return nil, err
	}
	parts, ok := content.Parts()
	if !ok {
		return nil, errf(errBadRequest, "message does not contain tool calls")
	}
	for _, p := range parts {
		if (p.Type == PartToolCall || p.Type == PartMCPToolCall) && p.ID == toolCallID {
			return parts, nil
		}
	}
	return nil, errf(errNotFound, "tool call not found")
}

// findToolCall mirrors find_tool_call — returns (name, args) of the call.
func findToolCall(parts []MessagePart, toolCallID string) (string, json.RawMessage, bool) {
	for _, p := range parts {
		if (p.Type == PartToolCall || p.Type == PartMCPToolCall) && p.ID == toolCallID {
			return p.Name, p.JSON, true
		}
	}
	return "", nil, false
}

// updateToolCallArgs mirrors update_tool_call_args.
func updateToolCallArgs(parts []MessagePart, toolCallID string, args json.RawMessage) {
	for i := range parts {
		if (parts[i].Type == PartToolCall || parts[i].Type == PartMCPToolCall) && parts[i].ID == toolCallID {
			parts[i].JSON = args
			return
		}
	}
}

// updateToolResponseJSON mirrors update_tool_response_json.
func updateToolResponseJSON(parts []MessagePart, toolCallID string, response json.RawMessage) {
	for i := range parts {
		if parts[i].Type == PartToolCallResponseJSON && parts[i].ID == toolCallID {
			parts[i].JSON = response
			return
		}
	}
}

// toolMutation is the shared auth wrapper for the /tool/* routes — all of
// them use OwnerAccessLevel in the Rust router.
func (s *Service) toolMutation(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, chatID string, caller auth.Caller) (any, error)) {
	chatID := chi.URLParam(r, "chat_id")
	caller, authed := auth.FromContext(r.Context())
	if !authed {
		writeTextErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.requireAccess(r.Context(), caller, authed, chatID, AccessLevelOwner); err != nil {
		writeAccessErr(w, err)
		return
	}
	result, err := fn(r.Context(), chatID, caller)
	if err != nil {
		writeChatErr(w, err)
		return
	}
	if result != nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// updateToolCall handles POST /chats/{chat_id}/tool/update.
func (s *Service) updateToolCall(w http.ResponseWriter, r *http.Request) {
	var req updateToolCallRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	s.toolMutation(w, r, func(ctx context.Context, chatID string, caller auth.Caller) (any, error) {
		parts, err := s.toolCallParts(ctx, chatID, req.MessageID, req.ToolCallID)
		if err != nil {
			return nil, err
		}
		// TODO(tools): Rust validates args against the toolset schema
		// (toolset.is_valid_tool) — add a Validate method to ToolRunner when
		// the ai_tools port lands.
		updateToolCallArgs(parts, req.ToolCallID, req.Args)
		// update_interim_message_content: persist without bumping chat recency.
		return nil, s.repo.updateMessageContent(ctx, chatID, req.MessageID, PartsContent(parts), false)
	})
}

// updateToolResponse handles POST /chats/{chat_id}/tool/response/update.
func (s *Service) updateToolResponse(w http.ResponseWriter, r *http.Request) {
	var req updateToolResponseRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	s.toolMutation(w, r, func(ctx context.Context, chatID string, caller auth.Caller) (any, error) {
		parts, err := s.toolCallParts(ctx, chatID, req.MessageID, req.ToolCallID)
		if err != nil {
			return nil, err
		}
		// `response` is a UserToolResponse<Value> — stored verbatim.
		updateToolResponseJSON(parts, req.ToolCallID, req.Response)
		return nil, s.repo.updateMessageContent(ctx, chatID, req.MessageID, PartsContent(parts), true)
	})
}

// callTool handles POST /chats/{chat_id}/tool/call — executes the tool and
// records the response part. Requires a configured ToolRunner; without one
// the endpoint reports 501 (the ai_tools port is not wired yet).
func (s *Service) callTool(w http.ResponseWriter, r *http.Request) {
	var req callToolRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	s.toolMutation(w, r, func(ctx context.Context, chatID string, caller auth.Caller) (any, error) {
		if s.tools == nil {
			return nil, errf(errNotImplemented, "tool execution is not configured")
		}
		parts, err := s.toolCallParts(ctx, chatID, req.MessageID, req.ToolCallID)
		if err != nil {
			return nil, err
		}
		toolName, storedArgs, _ := findToolCall(parts, req.ToolCallID)
		args := storedArgs
		if req.Args != nil {
			// Persist the updated args first (mirrors call_tool's
			// update_tool_call call).
			updateToolCallArgs(parts, req.ToolCallID, *req.Args)
			if err := s.repo.updateMessageContent(ctx, chatID, req.MessageID, PartsContent(parts), false); err != nil {
				return nil, wrapErr(errInternal, "update tool call", err)
			}
			args = *req.Args
		}

		result, err := s.tools.Call(ctx, caller.UserID, toolName, args)
		responseJSON := result
		if err != nil {
			// Tool-level errors persist as {"error": msg} (mirrors the
			// tool_err → {"error": description} mapping).
			responseJSON, _ = json.Marshal(map[string]string{"error": err.Error()})
		}

		// Re-fetch and record the response part.
		parts, err = s.toolCallParts(ctx, chatID, req.MessageID, req.ToolCallID)
		if err != nil {
			return nil, err
		}
		updateToolResponseJSON(parts, req.ToolCallID, responseJSON)
		if err := s.repo.updateMessageContent(ctx, chatID, req.MessageID, PartsContent(parts), true); err != nil {
			return nil, wrapErr(errInternal, "update tool response", err)
		}
		return map[string]json.RawMessage{"result": responseJSON}, nil
	})
}

// rejectToolCall handles POST /chats/{chat_id}/tool/reject — writes the
// UserToolResponse::Rejected marker ("Rejected" over the wire).
func (s *Service) rejectToolCall(w http.ResponseWriter, r *http.Request) {
	var req rejectToolCallRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	s.toolMutation(w, r, func(ctx context.Context, chatID string, caller auth.Caller) (any, error) {
		parts, err := s.toolCallParts(ctx, chatID, req.MessageID, req.ToolCallID)
		if err != nil {
			return nil, err
		}
		updateToolResponseJSON(parts, req.ToolCallID, json.RawMessage(`"Rejected"`))
		return nil, s.repo.updateMessageContent(ctx, chatID, req.MessageID, PartsContent(parts), true)
	})
}
