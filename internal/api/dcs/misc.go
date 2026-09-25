// Misc DCS endpoints — structured completion, the OpenAI chat-completions
// proxy, citations, batch preview, attachment→chats lookup, and id_mapping.
package dcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/macro-inc/macro/internal/api/httpx"
)

// ---------------------------------------------------------------------------
// POST /structured-completion
// ---------------------------------------------------------------------------

// structuredCompletion mirrors api/structured_completion.rs: phase 1 runs the
// agent loop over the prompt (tool access per toolset), phase 2 asks for a
// schema-constrained response. The model is always the caller's best model —
// request.model is intentionally ignored, matching the Rust handler.
func (s *Service) structuredCompletion(w http.ResponseWriter, r *http.Request) {
	caller, userID, ok := callerUserID(w, r)
	if !ok {
		return
	}
	var req StructuredCompletionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	pro, err := s.modelAccessFor(r.Context(), caller)
	if err != nil {
		writeObjErr(w, http.StatusInternalServerError, "failed to resolve model access")
		return
	}
	model := bestModel(pro)

	toolset := req.ToolSet.orDefault()
	system := s.toolsetPrompt(r.Context(), toolset)
	if req.AdditionalInstructions != nil && *req.AdditionalInstructions != "" {
		system += "\n" + *req.AdditionalInstructions
	}

	userMsg := storedMessage{role: RoleUser, content: TextContent(req.Prompt)}

	// Phase 1: gather context. With no tool runner configured this degrades to
	// a plain completion — tool calls are parsed but never executed, matching
	// the "no tools" toolset behavior.
	var gathered []MessagePart
	var toolSpecs []ToolSpec
	if toolset == ToolSetAll && s.tools != nil {
		if specs, err := s.tools.Definitions(r.Context(), userID); err == nil {
			toolSpecs = specs
		}
	}
	parts, _, err := s.ai.StreamChat(r.Context(), &ChatRequest{
		Model:    model,
		Messages: toProviderMessages([]storedMessage{userMsg}, nil),
		System:   system,
		Stream:   true,
		Tools:    toolSpecs,
	}, func(part MessagePart) error { return nil })
	if err != nil {
		slog.Error("dcs: structured completion phase 1", "err", err)
		writeObjErr(w, http.StatusInternalServerError, fmt.Sprintf("Agent loop failed: %v", err))
		return
	}
	gathered = mergeConsecutiveParts(parts)

	// Phase 2: schema-constrained completion over the gathered conversation.
	conversation := []storedMessage{
		userMsg,
		{role: RoleAssistant, content: PartsContent(gathered)},
		{role: RoleUser, content: TextContent("Based on the information gathered above, produce a structured response matching the required schema.")},
	}
	result, err := s.ai.StructuredOutput(r.Context(), &ChatRequest{
		Model:    model,
		Messages: toProviderMessages(conversation, nil),
		System:   system,
	}, req.OutputSchema)
	if err != nil {
		slog.Error("dcs: structured completion phase 2", "err", err)
		writeObjErr(w, http.StatusInternalServerError, fmt.Sprintf("Structured completion failed: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, StructuredCompletionResponse{Result: result})
}

// ---------------------------------------------------------------------------
// POST /chat/completions — OpenAI-compatible passthrough proxy
// ---------------------------------------------------------------------------

// chatCompletions mirrors api/completions.rs: force stream=false, forward the
// body to OpenAI with the configured key, and relay status + body verbatim.
func (s *Service) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := callerUserID(w, r); !ok {
		return
	}
	var body map[string]any
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	body["stream"] = false
	payload, err := json.Marshal(body)
	if err != nil {
		writeTextErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.cfg.OpenAIBaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		writeTextErr(w, http.StatusBadGateway, err.Error())
		return
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+s.cfg.OpenAIAPIKey)

	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		slog.Error("dcs: proxy chat completion", "err", err)
		writeTextErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeTextErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// ---------------------------------------------------------------------------
// GET /citations/{id} — optional auth (mirrors OptionalMacroAuthorization)
// ---------------------------------------------------------------------------

func (s *Service) getCitation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var reference string
	var documentID string
	err := s.repo.pool.QueryRow(r.Context(), `
		SELECT reference, "documentId" FROM "DocumentTextParts" WHERE id = $1
	`, id).Scan(&reference, &documentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeTextErr(w, http.StatusNotFound, "not found - possible hallucination")
			return
		}
		slog.Error("dcs: get citation", "err", err, "id", id)
		writeTextErr(w, http.StatusInternalServerError, "unable to get citation")
		return
	}
	writeJSON(w, http.StatusOK, DocumentTextPart{
		ID:         id,
		DocumentID: documentID,
		Reference:  json.RawMessage(reference),
	})
}

// ---------------------------------------------------------------------------
// POST /preview — batch chat previews; anonymous callers allowed
// ---------------------------------------------------------------------------

func (s *Service) getBatchPreview(w http.ResponseWriter, r *http.Request) {
	var req GetBatchPreviewRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	// Dedup to prevent duplicate work (mirrors the HashSet in Rust).
	seen := map[string]bool{}
	var ids []string
	for _, id := range req.ChatIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	rows, err := s.repo.pool.Query(r.Context(), `
		SELECT c.id, c.name, c."userId", c."updatedAt"::timestamptz
		FROM "Chat" c WHERE c."id" = ANY($1)
	`, ids)
	if err != nil {
		slog.Error("dcs: batch preview", "err", err)
		writeTextErr(w, http.StatusInternalServerError, "unable to get chat previews")
		return
	}
	defer rows.Close()
	found := map[string]ChatPreview{}
	for rows.Next() {
		var p ChatPreview
		var updated pgtype.Timestamptz
		if err := rows.Scan(&p.ChatID, &p.ChatName, &p.Owner, &updated); err != nil {
			slog.Error("dcs: batch preview scan", "err", err)
			writeTextErr(w, http.StatusInternalServerError, "unable to get chat previews")
			return
		}
		p.Type = "access"
		p.UpdatedAt = ts(updated)
		found[p.ChatID] = p
	}
	previews := make([]ChatPreview, 0, len(ids))
	for _, id := range ids {
		if p, ok := found[id]; ok {
			previews = append(previews, p)
		} else {
			previews = append(previews, ChatPreview{Type: "does_not_exist", ChatID: id})
		}
	}
	writeJSON(w, http.StatusOK, GetBatchPreviewResponse{Previews: previews})
}

// ---------------------------------------------------------------------------
// GET /attachments/{attachment_id}/chats
// ---------------------------------------------------------------------------

func (s *Service) getChatsForAttachment(w http.ResponseWriter, r *http.Request) {
	_, userID, ok := callerUserID(w, r)
	if !ok {
		return
	}
	attachmentID := chi.URLParam(r, "attachment_id")
	recent, all, err := s.repo.attachmentChats(r.Context(), attachmentID, userID)
	if err != nil {
		slog.Error("dcs: chats for attachment", "err", err, "attachment", attachmentID)
		writeTextErr(w, http.StatusInternalServerError, "unable to get chats for attachment")
		return
	}
	if all == nil {
		all = []Chat{}
	}
	writeJSON(w, http.StatusOK, GetChatsForAttachmentResponse{RecentChat: recent, AllChats: all})
}

// ---------------------------------------------------------------------------
// /id_mapping/{source_id} — auth required (UserOrInternal)
// ---------------------------------------------------------------------------

func (s *Service) createIDMappingHandler(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := callerUserID(w, r); !ok {
		return
	}
	sourceID := chi.URLParam(r, "source_id")
	var req CreateIdMappingRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if err := s.repo.createIDMapping(r.Context(), sourceID, req.TargetID); err != nil {
		slog.Error("dcs: create id mapping", "err", err, "source", sourceID)
		writeTextErr(w, http.StatusInternalServerError, "unable to create id mapping")
		return
	}
	writeJSON(w, http.StatusCreated, CreateIdMappingResponse{Success: true})
}

func (s *Service) getIDMappingHandler(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := callerUserID(w, r); !ok {
		return
	}
	sourceID := chi.URLParam(r, "source_id")
	target, err := s.repo.getIDMapping(r.Context(), sourceID)
	if err != nil {
		slog.Error("dcs: get id mapping", "err", err, "source", sourceID)
		writeTextErr(w, http.StatusInternalServerError, "unable to get id mapping")
		return
	}
	writeJSON(w, http.StatusOK, GetIdMappingResponse{TargetID: target})
}
