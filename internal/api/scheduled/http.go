package scheduled

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/config"
)

// Config carries the scheduled-action service settings; it embeds the root
// config so package-level env additions live here, not in pkg/config.
type Config struct {
	config.Config
}

// Deps wires the router.
type Deps struct {
	Service *Service
}

// Register mounts the axum_router.rs surface (already under the caller's
// mount prefix): /scheduled-actions CRUD+execute+history and /health.
func (d Deps) Register(r chi.Router) {
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
	})
	d.RegisterActions(r)
}

// RegisterActions mounts only the /scheduled-actions routes (the Rust
// service dual-mounted them at root and under /scheduled-action; the
// combined binary registers them at root without a second /health).
func (d Deps) RegisterActions(r chi.Router) {
	r.Get("/scheduled-actions", d.listActions)
	r.Post("/scheduled-actions", d.createAction)
	r.Put("/scheduled-actions/{id}", d.updateAction)
	r.Delete("/scheduled-actions/{id}", d.deleteAction)
	r.Post("/scheduled-actions/{id}/execute", d.executeAction)
	r.Get("/scheduled-actions/{id}/history", d.getHistory)
}

func caller(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := auth.FromContext(r.Context())
	if !ok || c.UserID == "" {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return c.UserID, true
}

func writeServiceErr(w http.ResponseWriter, err error) {
	var already *AlreadyRunningError
	var ownerNotUser *OwnerNotUserError
	var notFound *NotFoundError
	switch {
	case errors.As(err, &already):
		httpx.Error(w, http.StatusConflict, err.Error())
	case errors.As(err, &ownerNotUser):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &notFound):
		httpx.Error(w, http.StatusNotFound, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, err.Error())
	}
}

func (d Deps) listActions(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	actions, err := d.Service.GetActions(r.Context(), user)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, actions)
}

func (d Deps) createAction(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	var req CreateScheduledAction
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	next, err := nextRunAfterNow(req.Schedule, req.Timezone, time.Now().UTC())
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	action := ScheduledAction{
		Owner:     user,
		Name:      req.Name,
		Schedule:  req.Schedule,
		Kind:      req.Kind,
		Timezone:  req.Timezone,
		Task:      req.Task,
		NextRunAt: next,
		Enabled:   req.Enabled,
	}
	created, err := d.Service.CreateAction(r.Context(), action)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, created)
}

func (d Deps) updateAction(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid action id")
		return
	}
	var req UpdateScheduledAction
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	next, err := nextRunAfterNow(req.Schedule, req.Timezone, time.Now().UTC())
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	action := ScheduledAction{
		ID:        id,
		Owner:     user,
		Name:      req.Name,
		Schedule:  req.Schedule,
		Kind:      req.Kind,
		Timezone:  req.Timezone,
		Task:      req.Task,
		NextRunAt: next,
		Enabled:   req.Enabled,
	}
	updated, err := d.Service.UpdateAction(r.Context(), action, user)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

func (d Deps) deleteAction(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid action id")
		return
	}
	if err := d.Service.DeleteAction(r.Context(), id, user); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) executeAction(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid action id")
		return
	}
	exec, err := d.Service.ExecuteActionNow(r.Context(), id, user)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, exec)
}

func (d Deps) getHistory(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid action id")
		return
	}
	records, err := d.Service.GetExecutionRecords(r.Context(), id, user)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, records)
}
