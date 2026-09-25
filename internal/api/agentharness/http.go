// Package agentharness is the Go port of services/agent_harness_service
// (session HTTP API) plus the usable core of crates/agent_session,
// crates/agent_harness, and crates/agent_trigger for the self-host build.
//
// Parity gaps (tracked as TODOs inline):
//   - The ACP/session protocol pump is a stub: control actions are logged and
//     forwarded best-effort to a sidecar or dialed-in harness; realtime
//     fanout, permission requests, and log folding are not ported.
//   - Entity access is reduced to owner + direct/channel entity_access rows;
//     project/team-grant expansion belongs to the entity_access port.
//   - Claude-auth routes, session sharing routes, agent-models probing, and
//     agent-repositories listing are not ported.
//   - The egress proxy (agent_egress) is not ported; sandboxes get
//     MACRO_EGRESS_URL only when one is configured.
package agentharness

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// Deps is the router's dependency bundle.
type Deps struct {
	Svc *Service
}

// Register mounts the agent-harness routes on r. The caller applies the user
// auth middleware — except /runtime/ws, which authenticates harness tokens
// itself and must not sit behind the user middleware.
func (d Deps) Register(r chi.Router) {
	r.Route("/agent-sessions", func(r chi.Router) {
		r.Post("/", d.createSession)
		r.Post("/preview", d.previewSessions)
		r.Get("/{session_id}", d.getSession)
		r.Get("/{session_id}/log", d.getSessionLog)
		r.Put("/{session_id}/name", d.renameSession)
		r.Delete("/{session_id}", d.deleteSession)
		r.Post("/{session_id}/control", d.controlSession)
		r.Get("/{session_id}/queue", d.getQueue)
		r.Put("/{session_id}/queue/{action_id}", d.editQueued)
		r.Delete("/{session_id}/queue/{action_id}", d.removeQueued)
		r.Put("/{session_id}/sandbox-size", d.putSessionSandboxSize)
	})
	r.Get("/agent-sandbox-size", d.getSandboxSize)
	r.Put("/agent-sandbox-size", d.putSandboxSize)
}

// RegisterRuntime mounts the harness dial-in gateway. NOT behind user auth —
// it authenticates x-macro-harness-token itself.
func (d Deps) RegisterRuntime(r chi.Router) {
	r.Get("/runtime/ws", d.Svc.runtimeWSHandler)
}

// ---------- handlers -------------------------------------------------------

func (d Deps) createSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req CreateSessionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	sess, err := d.Svc.Create(r.Context(), caller, req)
	if err != nil {
		var tee *ThreadExistsError
		switch {
		case errors.As(err, &tee):
			httpx.WriteJSON(w, http.StatusConflict, ThreadSessionExistsResponse{
				Message:   ErrThreadSessionExists.Error(),
				SessionID: &tee.Session.ID,
			})
		case errors.Is(err, ErrBotRequired):
			httpx.ErrorJSON(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrUnknownBot), errors.Is(err, ErrNotAgentBot),
			errors.Is(err, ErrNotYourBot):
			httpx.ErrorJSON(w, http.StatusForbidden, err.Error())
		case errors.Is(err, ErrUnarmed):
			httpx.ErrorJSON(w, http.StatusServiceUnavailable, err.Error())
		default:
			httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated,
		CreateSessionResponse{Session: toResponse(sess, "owner")})
}

func (d Deps) previewSessions(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req PreviewRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	resp := PreviewResponse{Sessions: []SessionResponse{}}
	for _, id := range req.SessionIDs {
		sess, level, err := d.Svc.GetForCaller(r.Context(), caller, id)
		if err != nil {
			continue // preview skips sessions the caller cannot see
		}
		resp.Sessions = append(resp.Sessions, toResponse(sess, level))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (d Deps) getSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sess, level, err := d.Svc.GetForCaller(r.Context(), caller, chi.URLParam(r, "session_id"))
	if writeErr(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(sess, level))
}

func (d Deps) getSessionLog(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := chi.URLParam(r, "session_id")
	if _, _, err := d.Svc.GetForCaller(r.Context(), caller, id); writeErr(w, err) {
		return
	}
	entries, err := d.Svc.repo.ListLog(r.Context(), id)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []SessionLogEntry{}
	}
	httpx.WriteJSON(w, http.StatusOK, LogResponse{Entries: entries})
}

func (d Deps) renameSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req RenameRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpx.ErrorJSON(w, http.StatusBadRequest, "name must not be blank")
		return
	}
	sess, err := d.Svc.requireEdit(r.Context(), caller, chi.URLParam(r, "session_id"))
	if writeErr(w, err) {
		return
	}
	if err := d.Svc.repo.Rename(r.Context(), sess.ID, name); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (d Deps) deleteSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := d.Svc.Delete(r.Context(), caller, chi.URLParam(r, "session_id")); writeErr(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (d Deps) controlSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req ControlRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	resp, err := d.Svc.Control(r.Context(), caller, chi.URLParam(r, "session_id"), req)
	if writeErr(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (d Deps) getQueue(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	entries, err := d.Svc.Queue(r.Context(), caller, chi.URLParam(r, "session_id"))
	if writeErr(w, err) {
		return
	}
	if entries == nil {
		entries = []QueuedAction{}
	}
	httpx.WriteJSON(w, http.StatusOK, QueueResponse{Entries: entries})
}

func (d Deps) editQueued(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req EditQueuedActionRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	err := d.Svc.EditQueuedAction(r.Context(), caller,
		chi.URLParam(r, "session_id"), chi.URLParam(r, "action_id"), req.Prompt)
	if writeErr(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (d Deps) removeQueued(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	err := d.Svc.RemoveQueuedAction(r.Context(), caller,
		chi.URLParam(r, "session_id"), chi.URLParam(r, "action_id"))
	if writeErr(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (d Deps) putSessionSandboxSize(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req SandboxSizeRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if !req.Size.valid() {
		writeErr(w, ErrInvalidSize)
		return
	}
	sess, err := d.Svc.requireEdit(r.Context(), caller, chi.URLParam(r, "session_id"))
	if writeErr(w, err) {
		return
	}
	if err := d.Svc.repo.SetSandboxSize(r.Context(), sess.ID, req.Size); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (d Deps) getSandboxSize(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	size, err := d.Svc.repo.UserSandboxSize(r.Context(), caller.UserID)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !size.valid() {
		size = SandboxDefault
	}
	httpx.WriteJSON(w, http.StatusOK, SandboxSizeResponse{Size: size})
}

func (d Deps) putSandboxSize(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req SandboxSizeRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if !req.Size.valid() {
		writeErr(w, ErrInvalidSize)
		return
	}
	if err := d.Svc.repo.SetUserSandboxSize(r.Context(), caller.UserID, req.Size); err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// ---------- helpers --------------------------------------------------------

func writeErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.ErrorJSON(w, http.StatusNotFound, "session not found")
	case errors.Is(err, ErrForbidden):
		httpx.ErrorJSON(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrInvalidSize):
		httpx.ErrorJSON(w, http.StatusBadRequest, err.Error())
	default:
		httpx.ErrorJSON(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

// toResponse renders the wire DTO; canEdit follows the caller's level.
func toResponse(s *Session, level string) SessionResponse {
	resp := SessionResponse{
		ID:                   s.ID,
		Name:                 s.Name,
		OwnerID:              s.OwnerID,
		CanEdit:              level == "owner" || level == "edit",
		ThreadID:             s.ThreadID,
		ThreadChannelID:      nil,
		OriginatingMessageID: s.OriginatingMessageID,
		BotID:                s.BotID,
		Model:                s.Model,
		Harness:              s.Harness,
		RepoURL:              s.RepoURL,
		PullRequestURL:       s.PullRequestURL,
		Workspace:            s.Workspace,
		SandboxSize:          s.SandboxSize,
		Instructions:         s.Instructions,
		ACPSessionID:         s.ACPSessionID,
		Status: SessionStatusDTO{
			Status:    s.Status,
			EventName: s.StatusEventName,
		},
		CreatedAt:  s.CreatedAt,
		ModifiedAt: s.ModifiedAt,
	}
	if s.ThreadParentID != nil && s.ThreadParentType != "" {
		resp.ThreadParent = &ThreadParentRef{
			EntityType: s.ThreadParentType,
			EntityID:   *s.ThreadParentID,
		}
		if s.ThreadParentType == "channel" {
			resp.ThreadChannelID = s.ThreadParentID
		}
	}
	if s.External != nil {
		resp.External = &ExternalResponse{
			Provider: s.External.Provider,
			Name:     s.External.ExternalName,
			URL:      s.External.ExternalURL,
		}
	}
	return resp
}
