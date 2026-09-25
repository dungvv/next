package notification

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// Handlers exposes the notification HTTP surface, mirroring
// services/notification_service + crates/notification/src/inbound/http.
type Handlers struct {
	svc        *Service
	pushEvents *PushEventHandler
	// internalKey guards the authenticated surface (x-internal-auth-key /
	// JWT). Required because Register applies auth itself — the presigned
	// GET below must stay reachable without credentials.
	internalKey string
	// signer verifies presigned preference-disable URLs (URL_SIGNING_HMAC).
	signer *URLSigner
	// publicURL is the configured public base URL of this service
	// (NOTIFICATION_SERVICE_URL) — the scheme/host used to canonicalize the
	// URL being verified.
	publicURL string
}

// Register installs all routes on r. Callers mount it at root and under
// /{version} to match the Rust dual-mount. Authenticated routes are wrapped
// in auth.Middleware here rather than at the server level so the presigned
// unsubscribe link works without credentials.
func (h Handlers) Register(r chi.Router) {
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// Public presigned preference-disable (Rust:
	// GET /user_notifications/preferences/{type}/disable). The request is
	// authorized by the URL's HMAC signature, not by auth middleware.
	r.Get("/user_notifications/preferences/{notification_event_type}/disable", h.presignedDisableType)

	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(h.internalKey))

		// Devices (Rust: /device/register, /device/unregister). The task's
		// REST-style aliases are also exposed.
		r.Route("/device", func(r chi.Router) {
			r.Post("/register", h.registerDevice)
			r.Delete("/unregister", h.unregisterDevice)
		})
		r.Post("/devices", h.registerDevice)
		r.Delete("/devices/{token}", h.unregisterDeviceByPath)

		r.Route("/user_notifications", func(r chi.Router) {
			r.Get("/", h.listNotifications)
			r.Route("/bulk", func(r chi.Router) {
				r.Delete("/", h.bulkDelete)
				r.Patch("/seen", h.bulkSeen)
				r.Patch("/done", h.bulkDone)
				r.Patch("/undone", h.bulkUndone)
			})
			r.Post("/item/bulk", h.bulkGetByEventItemIDs)
			r.Get("/item/{event_item_id}", h.getByEventItemID)
			r.Get("/preferences", h.getPreferences)
			r.Route("/preferences/{notification_event_type}", func(r chi.Router) {
				r.Put("/disable", h.disableType)
				r.Put("/enable", h.enableType)
			})
			r.Get("/{notification_id}", h.getByID)
			r.Delete("/{notification_id}", h.deleteByID)
		})

		r.Route("/unsubscribe", func(r chi.Router) {
			r.Get("/", h.listUnsubscribes)
			r.Post("/email", h.unsubscribeEmail)
			r.Post("/item/{item_type}/{item_id}", h.unsubscribeItem)
			r.Delete("/item/{item_type}/{item_id}", h.resubscribeItem)
			r.Post("/mute", h.mute)
			r.Delete("/mute", h.unmute)
		})

		// Provider feedback webhook (alternative to the push_events consumer).
		r.Post("/internal/push_events", h.pushEvent)
	})
}

func caller(w http.ResponseWriter, r *http.Request) (auth.Caller, bool) {
	c, ok := auth.FromContext(r.Context())
	if !ok || c.UserID == "" {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return auth.Caller{}, false
	}
	return c, true
}

// ---- devices ----

type deviceRequest struct {
	Token      string `json:"token"`
	DeviceType string `json:"device_type"`
}

func (h Handlers) registerDevice(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req deviceRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	dt, err := ParseDeviceType(req.DeviceType)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.RegisterDevice(r.Context(), c.UserID, req.Token, dt); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "unable to register device")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (h Handlers) unregisterDevice(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req deviceRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	h.unregister(w, r, c.UserID, req.Token, req.DeviceType)
}

func (h Handlers) unregisterDeviceByPath(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	h.unregister(w, r, c.UserID, chi.URLParam(r, "token"), r.URL.Query().Get("device_type"))
}

func (h Handlers) unregister(w http.ResponseWriter, r *http.Request, userID, token, deviceType string) {
	dt, err := ParseDeviceType(deviceType)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UnregisterDevice(r.Context(), userID, token, dt); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "unable to unregister device")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// ---- listing / lookup ----

type listResponse struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// parseStates parses the `states` query param: comma-separated exact states.
// Omitted → ActiveStates; empty string → no state filter (Rust contract).
func parseStates(q map[string][]string) []State {
	raw, present := q["states"]
	if !present || len(raw) == 0 {
		return ActiveStates
	}
	if raw[0] == "" {
		return nil
	}
	var out []State
	for _, s := range strings.Split(raw[0], ",") {
		switch State(strings.TrimSpace(s)) {
		case StateUnseen, StateSeen, StateDone:
			out = append(out, State(strings.TrimSpace(s)))
		}
	}
	return out
}

func parseLimit(r *http.Request) int {
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return 0
}

func (h Handlers) listQuery(w http.ResponseWriter, r *http.Request, c auth.Caller) (ListQuery, bool) {
	cur, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return ListQuery{}, false
	}
	q := ListQuery{
		UserID: c.UserID,
		Limit:  parseLimit(r),
		States: parseStates(r.URL.Query()),
	}
	if cur != nil {
		q.CursorID = &cur.ID
		ts := cur.Val.LastVal
		q.CursorTS = &ts
	}
	return q, true
}

func (h Handlers) respondList(w http.ResponseWriter, r *http.Request, q ListQuery) {
	rows, next, err := h.svc.ListNotifications(r.Context(), q)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to get notifications")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.wire())
	}
	resp := listResponse{Items: items}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h Handlers) listNotifications(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	q, ok := h.listQuery(w, r, c)
	if !ok {
		return
	}
	h.respondList(w, r, q)
}

type bulkGetByEventItemIDsRequest struct {
	EventItemIDs []string `json:"eventItemIds"`
}

func (h Handlers) bulkGetByEventItemIDs(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req bulkGetByEventItemIDsRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	q, ok := h.listQuery(w, r, c)
	if !ok {
		return
	}
	q.EventItemIDs = req.EventItemIDs
	h.respondList(w, r, q)
}

func (h Handlers) getByEventItemID(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	q, ok := h.listQuery(w, r, c)
	if !ok {
		return
	}
	q.EventItemIDs = []string{chi.URLParam(r, "event_item_id")}
	h.respondList(w, r, q)
}

func (h Handlers) getByID(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "notification_id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid notification id")
		return
	}
	row, err := h.svc.repo.GetUserNotificationByID(r.Context(), c.UserID, id)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to get notification")
		return
	}
	if row == nil {
		httpx.Error(w, http.StatusNotFound, "notification not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row.wire())
}

// ---- status + delete ----

type bulkRequest struct {
	NotificationIDs []uuid.UUID `json:"notificationIds"`
}

func (h Handlers) bulkUpdate(w http.ResponseWriter, r *http.Request, status NotificationStatus) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req bulkRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rows, err := h.svc.UpdateNotifications(r.Context(), c.UserID, req.NotificationIDs, status)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to update notifications")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.wire())
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

func (h Handlers) bulkSeen(w http.ResponseWriter, r *http.Request) {
	h.bulkUpdate(w, r, NotificationStatus{Kind: StatusSeen})
}

func (h Handlers) bulkDone(w http.ResponseWriter, r *http.Request) {
	h.bulkUpdate(w, r, NotificationStatus{Kind: StatusDone})
}

func (h Handlers) bulkUndone(w http.ResponseWriter, r *http.Request) {
	h.bulkUpdate(w, r, NotificationStatus{Kind: StatusUndone})
}

func (h Handlers) deleteByID(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "notification_id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid notification id")
		return
	}
	if err := h.svc.repo.DeleteUserNotification(r.Context(), c.UserID, id); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to delete notification")
		return
	}
	if err := h.svc.status.PublishDelete(c.UserID, id.String()); err != nil {
		// best-effort realtime delete propagation
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (h Handlers) bulkDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	var req bulkRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if err := h.svc.repo.BulkDeleteUserNotifications(r.Context(), c.UserID, req.NotificationIDs); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to delete notifications")
		return
	}
	for _, id := range req.NotificationIDs {
		_ = h.svc.status.PublishDelete(c.UserID, id.String())
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// ---- preferences ----

func (h Handlers) getPreferences(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	disabled, err := h.svc.repo.GetDisabledNotificationTypes(r.Context(), c.UserID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to get preferences")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"disabled_notification_types": disabled})
}

func (h Handlers) setType(w http.ResponseWriter, r *http.Request, disable bool) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	eventType := chi.URLParam(r, "notification_event_type")
	if !IsBlockable(eventType) {
		httpx.Error(w, http.StatusBadRequest, "notification type is not blockable")
		return
	}
	var err error
	if disable {
		err = h.svc.repo.DisableNotificationType(r.Context(), c.UserID, eventType)
	} else {
		err = h.svc.repo.EnableNotificationType(r.Context(), c.UserID, eventType)
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to update preference")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (h Handlers) disableType(w http.ResponseWriter, r *http.Request) { h.setType(w, r, true) }
func (h Handlers) enableType(w http.ResponseWriter, r *http.Request)  { h.setType(w, r, false) }

// ---- unsubscribe ----

func (h Handlers) listUnsubscribes(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	items, err := h.svc.repo.ListUnsubscribedItems(r.Context(), c.UserID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to list unsubscribes")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

func (h Handlers) unsubscribeItem(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	err := h.svc.repo.AddItemUnsubscribe(r.Context(), c.UserID,
		chi.URLParam(r, "item_id"), chi.URLParam(r, "item_type"))
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to unsubscribe")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (h Handlers) resubscribeItem(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	err := h.svc.repo.RemoveItemUnsubscribe(r.Context(), c.UserID, chi.URLParam(r, "item_id"))
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to resubscribe")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (h Handlers) mute(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	if err := h.svc.repo.MuteUser(r.Context(), c.UserID); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to mute notifications")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

func (h Handlers) unmute(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	if err := h.svc.repo.UnmuteUser(r.Context(), c.UserID); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to unmute notifications")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// unsubscribeEmail ports POST /unsubscribe/email: records the caller's
// email (user id minus the "macro|" prefix) in
// notification_email_unsubscribe.
func (h Handlers) unsubscribeEmail(w http.ResponseWriter, r *http.Request) {
	c, ok := caller(w, r)
	if !ok {
		return
	}
	if err := h.svc.repo.AddEmailUnsubscribe(r.Context(), emailPart(c.UserID)); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "unable to unsubscribe email")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// presignedDisableType ports the Rust presigned_disable_notification_type:
// an unauthenticated GET carrying id={macro_user_id}&sig={hmac} in the
// query. The signature is verified over the canonical public request URL
// ({configured scheme}://{request Host}{path}?{query}); on success the
// notification type is disabled for the signed user and an HTML
// confirmation is returned.
func (h Handlers) presignedDisableType(w http.ResponseWriter, r *http.Request) {
	writeHTML := func(status int, body string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	eventType := chi.URLParam(r, "notification_event_type")
	userID := r.URL.Query().Get("id")
	if userID == "" {
		writeHTML(http.StatusBadRequest, "Invalid link")
		return
	}
	base, err := url.Parse(h.publicURL)
	if err != nil {
		writeHTML(http.StatusBadRequest, "Invalid link")
		return
	}
	host := r.Host
	if host == "" {
		host = base.Host
	}
	toVerify, err := url.Parse(fmt.Sprintf("%s://%s%s", base.Scheme, host, r.URL.RequestURI()))
	if err != nil {
		writeHTML(http.StatusBadRequest, "Invalid link")
		return
	}
	if !h.signer.Verify(toVerify) {
		writeHTML(http.StatusBadRequest, "Invalid signature")
		return
	}
	if !IsBlockable(eventType) {
		writeHTML(http.StatusBadRequest, fmt.Sprintf("Cannot block notification type %s", eventType))
		return
	}
	if err := h.svc.repo.DisableNotificationType(r.Context(), userID, eventType); err != nil {
		writeHTML(http.StatusInternalServerError, "failed to disable notification type")
		return
	}
	writeHTML(http.StatusOK, fmt.Sprintf("You have been unsubscribed from %s", eventType))
}

// ---- push events webhook ----

func (h Handlers) pushEvent(w http.ResponseWriter, r *http.Request) {
	c, ok := auth.FromContext(r.Context())
	if !ok || !auth.RequireInternal(w, c) {
		return
	}
	if h.pushEvents == nil {
		httpx.Error(w, http.StatusNotImplemented, "push event handling not configured")
		return
	}
	var ev PushEvent
	if !httpx.DecodeJSON(w, r, &ev) {
		return
	}
	if err := h.pushEvents.Handle(r.Context(), ev); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to handle push event")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}
