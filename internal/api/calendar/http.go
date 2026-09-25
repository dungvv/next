package calendar

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/pkg/config"
)

// Config carries the calendar service settings; it embeds the root config so
// package-level env additions live here, not in pkg/config.
type Config struct {
	config.Config
	// WatchToken verifies Google push notifications (CALENDAR_WATCH_TOKEN);
	// empty disables the webhook (404), matching calendar_watch_config.
	WatchToken string `env:"CALENDAR_WATCH_TOKEN" envDefault:""`
	// SyncEnabled mirrors calendar_sync_enabled: when false the mutation
	// routes are not mounted, exactly like the Rust api_router.
	SyncEnabled bool `env:"CALENDAR_SYNC_ENABLED" envDefault:"false"`
}

// Load parses environment variables into the calendar Config.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Deps wires the router.
type Deps struct {
	Service *Service
	Config  Config
}

// Register mounts the full calendar surface (already under the caller's
// `/calendar` mount prefix): the unauthenticated Google watch webhook plus
// the user-authenticated reads and mutations.
func (d Deps) Register(r chi.Router) {
	d.RegisterPublic(r)
	d.RegisterPrivate(r)
}

// RegisterPublic mounts the token-verified Google watch webhook — it must
// stay outside the auth middleware group (Google calls it directly).
func (d Deps) RegisterPublic(r chi.Router) {
	r.Post("/notifications", d.watchNotification)
}

// RegisterPrivate mounts the user-facing routes; callers wrap with
// auth.Middleware. The occurrence read routes are also registered at root in
// the combined binary (Rust served them from document_storage_service).
func (d Deps) RegisterPrivate(r chi.Router) {
	r.Get("/calendar-events", d.listOccurrences)
	r.Get("/calendar-events/", d.listOccurrences)
	r.Post("/calendar-events/preview", d.mentionPreviews)
	r.Get("/calendar-events/team-out-of-office", d.listTeamOutOfOffice)
	if d.Config.SyncEnabled {
		r.Get("/calendars", d.listCalendars)
		r.Post("/events", d.createEvent)
		r.Patch("/events/{event_id}", d.updateEvent)
		r.Delete("/events/{event_id}", d.deleteEvent)
		r.Put("/events/{event_id}/rsvp", d.rsvpEvent)
	}
}

func caller(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := auth.FromContext(r.Context())
	if !ok || c.UserID == "" {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return c.UserID, true
}

// ---------------------------------------------------------------------------
// occurrence reads (axum_router.rs)
// ---------------------------------------------------------------------------

type occurrenceQuery struct {
	start     time.Time
	end       time.Time
	startDate *Date
	endDate   *Date
	limit     int
	cursor    string
}

func parseTimeParam(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, v)
	}
	return t, err == nil
}

func parseDateParam(v string) (*Date, error) {
	if v == "" {
		return nil, nil
	}
	d, err := ParseDate(v)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func parseOccurrenceQuery(r *http.Request) (occurrenceQuery, *CalendarOccurrenceCursor, int, error) {
	q := r.URL.Query()
	start, ok := parseTimeParam(q.Get("start"))
	if !ok {
		return occurrenceQuery{}, nil, 0, errInvalidRange
	}
	end, ok := parseTimeParam(q.Get("end"))
	if !ok {
		return occurrenceQuery{}, nil, 0, errInvalidRange
	}
	startDate, err := parseDateParam(q.Get("startDate"))
	if err != nil {
		return occurrenceQuery{}, nil, 0, errInvalidRange
	}
	endDate, err := parseDateParam(q.Get("endDate"))
	if err != nil {
		return occurrenceQuery{}, nil, 0, errInvalidRange
	}
	limit := 1000
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 2000 {
			return occurrenceQuery{}, nil, 0, &ValidationError{"calendar limit must be between 1 and 2000"}
		}
		limit = n
	}
	var cursor *CalendarOccurrenceCursor
	if raw := q.Get("cursor"); raw != "" {
		c, err := DecodeCursor(raw)
		if err != nil {
			return occurrenceQuery{}, nil, 0, &ValidationError{"calendar cursor is invalid"}
		}
		cursor = c
	}
	return occurrenceQuery{start: start, end: end, startDate: startDate, endDate: endDate, limit: limit, cursor: q.Get("cursor")}, cursor, limit, nil
}

// defaultEndDate mirrors axum_router::default_end_date: a midnight `end`
// already lands on the right exclusive date; otherwise take the next day.
func defaultEndDate(end time.Time) Date {
	d := Date{time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)}
	if end.Hour() == 0 && end.Minute() == 0 && end.Second() == 0 && end.Nanosecond() == 0 {
		return d
	}
	return Date{d.Time.AddDate(0, 0, 1)}
}

func occurrenceRangeOf(q occurrenceQuery) (OccurrenceRange, error) {
	startDate := q.startDate
	if startDate == nil {
		d := Date{time.Date(q.start.Year(), q.start.Month(), q.start.Day(), 0, 0, 0, 0, time.UTC)}
		startDate = &d
	}
	endDate := q.endDate
	if endDate == nil {
		d := defaultEndDate(q.end)
		endDate = &d
	}
	return OccurrenceRange{
		StartsAt:  q.start,
		EndsAt:    q.end,
		StartDate: *startDate,
		EndDate:   *endDate,
	}, nil
}

type occurrenceItem struct {
	Event      CalendarEvent      `json:"event"`
	Occurrence CalendarOccurrence `json:"occurrence"`
}

type occurrenceResponse struct {
	Items      []occurrenceItem   `json:"items"`
	HasMore    bool               `json:"hasMore"`
	NextCursor *string            `json:"nextCursor"`
	SyncStatus CalendarSyncStatus `json:"syncStatus"`
}

func writeReadErr(w http.ResponseWriter, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		httpx.ErrorJSON(w, http.StatusBadRequest,
			"calendar range must be positive, at most 370 days, inside the maintained one-year-history/two-year-future window, with limit 1–2000")
		return
	}
	httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to query calendar occurrences")
}

func (d Deps) listOccurrences(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	q, cursor, limit, err := parseOccurrenceQuery(r)
	if err != nil {
		writeReadErr(w, err)
		return
	}
	rng, err := occurrenceRangeOf(q)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "calendar end is outside the supported date range")
		return
	}
	rows, err := d.Service.ListOccurrences(r.Context(), user, rng, cursor, limit+1)
	if err != nil {
		writeReadErr(w, err)
		return
	}
	syncStatus, err := d.Service.SyncStatus(r.Context(), user)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to query calendar occurrences")
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]occurrenceItem, 0, len(rows))
	var next *string
	for i := range rows {
		items = append(items, occurrenceItem{Event: rows[i].Event, Occurrence: rows[i].Occurrence})
	}
	if hasMore && len(rows) > 0 {
		if enc, err := CursorFromOccurrence(&rows[len(rows)-1].Occurrence).Encode(); err == nil {
			next = &enc
		}
	}
	httpx.WriteJSON(w, http.StatusOK, occurrenceResponse{
		Items: items, HasMore: hasMore, NextCursor: next, SyncStatus: syncStatus,
	})
}

type teamOutOfOfficeItem struct {
	OwnerID       string    `json:"ownerId"`
	EventID       uuid.UUID `json:"eventId"`
	OccurrenceKey string    `json:"occurrenceKey"`
	Title         *string   `json:"title,omitempty"`
	Time          EventTime `json:"time"`
}

type teamOutOfOfficeResponse struct {
	Items   []teamOutOfOfficeItem `json:"items"`
	HasMore bool                  `json:"hasMore"`
}

func (d Deps) listTeamOutOfOffice(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	q, _, limit, err := parseOccurrenceQuery(r)
	if err != nil {
		writeReadErr(w, err)
		return
	}
	rng, err := occurrenceRangeOf(q)
	if err != nil {
		httpx.ErrorJSON(w, http.StatusBadRequest, "calendar end is outside the supported date range")
		return
	}
	rows, err := d.Service.ListTeamOutOfOffice(r.Context(), user, rng, limit+1)
	if err != nil {
		writeReadErr(w, err)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]teamOutOfOfficeItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, teamOutOfOfficeItem{
			OwnerID: row.OwnerID, EventID: row.EventID,
			OccurrenceKey: row.OccurrenceKey, Title: row.Title, Time: row.Time,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, teamOutOfOfficeResponse{Items: items, HasMore: hasMore})
}

type mentionPreviewRequestItem struct {
	EventID       uuid.UUID `json:"eventId"`
	OccurrenceKey *string   `json:"occurrenceKey"`
}

type mentionPreviewRequest struct {
	Items []mentionPreviewRequestItem `json:"items"`
}

type mentionPreviewItem struct {
	EventID uuid.UUID             `json:"eventId"`
	Kind    string                `json:"type"`
	Event   *CalendarMentionEvent `json:"event,omitempty"`
}

type mentionPreviewResponse struct {
	Items []mentionPreviewItem `json:"items"`
}

func (d Deps) mentionPreviews(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	var req mentionPreviewRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	items := make([]CalendarMentionRequestItem, len(req.Items))
	ids := make([]uuid.UUID, len(req.Items))
	for i, it := range req.Items {
		items[i] = CalendarMentionRequestItem{EventID: it.EventID, OccurrenceKey: it.OccurrenceKey}
		ids[i] = it.EventID
	}
	previews, err := d.Service.MentionPreviews(r.Context(), user, items)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			httpx.ErrorJSON(w, http.StatusBadRequest, ve.Message)
			return
		}
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to resolve calendar mention previews")
		return
	}
	if len(previews) != len(ids) {
		httpx.ErrorJSON(w, http.StatusInternalServerError, "unable to resolve calendar mention previews")
		return
	}
	out := make([]mentionPreviewItem, len(previews))
	for i, p := range previews {
		item := mentionPreviewItem{EventID: ids[i]}
		switch p.Kind {
		case MentionAccessible:
			item.Kind = "access"
			item.Event = p.Event
		case MentionNoAccess:
			item.Kind = "no_access"
		default:
			item.Kind = "does_not_exist"
		}
		out[i] = item
	}
	httpx.WriteJSON(w, http.StatusOK, mentionPreviewResponse{Items: out})
}

// ---------------------------------------------------------------------------
// mutations (mutation_router.rs)
// ---------------------------------------------------------------------------

type attendeeInputBody struct {
	Email      string `json:"email"`
	IsOptional bool   `json:"isOptional"`
}

func (b attendeeInputBody) input() CalendarAttendeeInput {
	return CalendarAttendeeInput{Email: b.Email, IsOptional: b.IsOptional}
}

type createEventRequest struct {
	CalendarID      *uuid.UUID             `json:"calendarId"`
	EmailLinkID     *uuid.UUID             `json:"emailLinkId"`
	Title           string                 `json:"title"`
	Description     *string                `json:"description"`
	Location        *string                `json:"location"`
	Time            EventTime              `json:"time"`
	Attendees       []attendeeInputBody    `json:"attendees"`
	RecurrenceLines []string               `json:"recurrenceLines"`
	Visibility      *EventVisibility       `json:"visibility"`
	Transparency    *EventTransparency     `json:"transparency"`
	Reminders       *EventReminders        `json:"reminders"`
	Conference      *ConferenceChange      `json:"conference"`
	OutOfOffice     *OutOfOfficeProperties `json:"outOfOffice"`
}

type updateEventRequest struct {
	CalendarID      *uuid.UUID             `json:"calendarId"`
	Title           *string                `json:"title"`
	Description     *string                `json:"description"`
	Location        *string                `json:"location"`
	Time            *EventTime             `json:"time"`
	Attendees       *[]attendeeInputBody   `json:"attendees"`
	RecurrenceLines *[]string              `json:"recurrenceLines"`
	Visibility      *EventVisibility       `json:"visibility"`
	Transparency    *EventTransparency     `json:"transparency"`
	Reminders       *EventReminders        `json:"reminders"`
	Conference      *ConferenceChange      `json:"conference"`
	OutOfOffice     *OutOfOfficeProperties `json:"outOfOffice"`
	Scope           *string                `json:"scope"` // "all" | "this_event"
	RecurrenceID    *string                `json:"recurrenceId"`
}

type rsvpRequest struct {
	CalendarID   *uuid.UUID             `json:"calendarId"`
	Response     AttendeeResponseStatus `json:"response"`
	Scope        *string                `json:"scope"` // "all" | "this_event"
	RecurrenceID *string                `json:"recurrenceId"`
}

// mutationErrorBody mirrors CalendarMutationApiError on the wire.
type mutationErrorBody struct {
	Code    MutationErrorCode `json:"code"`
	Message string            `json:"message"`
}

func writeMutationErr(w http.ResponseWriter, err error) {
	var me *MutationError
	if !errors.As(err, &me) {
		httpx.WriteJSON(w, http.StatusInternalServerError, mutationErrorBody{
			Code: ErrCodeRetryable, Message: "the calendar mutation failed transiently; try again",
		})
		return
	}
	status := http.StatusInternalServerError
	message := me.Message
	switch me.Code {
	case ErrCodeNotFound, ErrCodeOccurrenceGone:
		status = http.StatusNotFound
	case ErrCodeReadOnly:
		status = http.StatusForbidden
	case ErrCodeNoWritable, ErrCodeNotAttendee:
		status = http.StatusConflict
	case ErrCodeInvalidInput:
		status = http.StatusBadRequest
	case ErrCodeReauthRequired:
		status, message = http.StatusForbidden, "calendar access must be re-authorized"
	case ErrCodeProviderRejected:
		status = http.StatusConflict
	case ErrCodeRetryable:
		status, message = http.StatusServiceUnavailable, "the calendar mutation failed transiently; try again"
	case ErrCodePersistFailed:
		status, message = http.StatusInternalServerError, "the change reached the calendar provider; refresh to see it"
	}
	httpx.WriteJSON(w, status, mutationErrorBody{Code: me.Code, Message: message})
}

// updateScopeOf mirrors mutation_router::update_scope.
func updateScopeOf(scope, recurrenceID *string) (UpdateScope, error) {
	s := ""
	if scope != nil {
		s = *scope
	}
	switch {
	case s == "this_event" && recurrenceID == nil:
		return UpdateScope{}, mutationErr(ErrCodeInvalidInput, "a this-event update requires recurrenceId")
	case s == "all" && recurrenceID != nil:
		return UpdateScope{}, mutationErr(ErrCodeInvalidInput, "recurrenceId only applies to a this_event update")
	case s == "this_event" || (s == "" && recurrenceID != nil):
		return UpdateScope{ThisEvent: recurrenceID}, nil
	default:
		return UpdateScope{}, nil
	}
}

func (d Deps) listCalendars(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	calendars, err := d.Service.ListVisibleCalendars(r.Context(), user)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"calendars": calendars})
}

func (d Deps) createEvent(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	var req createEventRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	draft := CalendarEventDraft{
		Title: req.Title, Description: req.Description, Location: req.Location,
		Time: req.Time, RecurrenceLines: req.RecurrenceLines,
		Visibility: req.Visibility, Transparency: req.Transparency,
		Reminders: req.Reminders, Conference: req.Conference, OutOfOffice: req.OutOfOffice,
	}
	for _, a := range req.Attendees {
		draft.Attendees = append(draft.Attendees, a.input())
	}
	event, err := d.Service.CreateEvent(r.Context(), user, req.EmailLinkID, req.CalendarID, draft)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, event)
}

func (d Deps) updateEvent(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "event_id"))
	if err != nil {
		writeMutationErr(w, ErrMutationNotFound)
		return
	}
	var req updateEventRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	scope, err := updateScopeOf(req.Scope, req.RecurrenceID)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	patch := CalendarEventPatch{
		Title: req.Title, Description: req.Description, Location: req.Location,
		Time: req.Time, RecurrenceLines: req.RecurrenceLines,
		Visibility: req.Visibility, Transparency: req.Transparency,
		Reminders: req.Reminders, Conference: req.Conference, OutOfOffice: req.OutOfOffice,
	}
	if req.Attendees != nil {
		list := make([]CalendarAttendeeInput, 0, len(*req.Attendees))
		for _, a := range *req.Attendees {
			list = append(list, a.input())
		}
		patch.Attendees = &list
	}
	event, err := d.Service.UpdateEvent(r.Context(), user, eventID, req.CalendarID, patch, scope)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, event)
}

func (d Deps) deleteEvent(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "event_id"))
	if err != nil {
		writeMutationErr(w, ErrMutationNotFound)
		return
	}
	q := r.URL.Query()
	kind := q.Get("scope")
	if kind == "" {
		kind = "all"
	}
	recurrenceID := q.Get("recurrenceId")
	if (kind == "this_event" || kind == "this_and_following") && recurrenceID == "" {
		writeMutationErr(w, mutationErr(ErrCodeInvalidInput,
			"a "+strings.ReplaceAll(kind, "_", "-")+" deletion requires recurrenceId"))
		return
	}
	var calID *uuid.UUID
	if raw := q.Get("calendarId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeMutationErr(w, mutationErr(ErrCodeInvalidInput, "invalid calendarId"))
			return
		}
		calID = &id
	}
	if err := d.Service.DeleteEvent(r.Context(), user, eventID, calID,
		DeletionScope{Kind: kind, RecurrenceID: recurrenceID}); err != nil {
		writeMutationErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) rsvpEvent(w http.ResponseWriter, r *http.Request) {
	user, ok := caller(w, r)
	if !ok {
		return
	}
	eventID, err := uuid.Parse(chi.URLParam(r, "event_id"))
	if err != nil {
		writeMutationErr(w, ErrMutationNotFound)
		return
	}
	var req rsvpRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	// Mirrors rsvp_calendar_event's scope resolution: an explicit `all` wins
	// over a dangling recurrenceId.
	var scope RsvpScope
	s := ""
	if req.Scope != nil {
		s = *req.Scope
	}
	switch {
	case s == "all":
	case s == "this_event" && req.RecurrenceID == nil:
		writeMutationErr(w, mutationErr(ErrCodeInvalidInput, "a this-event response requires recurrenceId"))
		return
	case s == "this_event" || (s == "" && req.RecurrenceID != nil):
		scope = RsvpScope{ThisEvent: req.RecurrenceID}
	}
	event, err := d.Service.RespondToEvent(r.Context(), user, eventID, req.CalendarID, req.Response, scope)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, event)
}

// ---------------------------------------------------------------------------
// Google watch webhook (calendar_watch.rs) — unauthenticated.
// ---------------------------------------------------------------------------

func (d Deps) watchNotification(w http.ResponseWriter, r *http.Request) {
	if d.Config.WatchToken == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !ConstantTimeTokenEqual(r.Header.Get("x-goog-channel-token"), d.Config.WatchToken) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Header.Get("x-goog-resource-state") == "sync" {
		w.WriteHeader(http.StatusOK)
		return
	}
	channelID := r.Header.Get("x-goog-channel-id")
	resourceID := r.Header.Get("x-goog-resource-id")
	if channelID == "" || resourceID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The poll is the backstop; unmatched or failed notifications are
	// acknowledged rather than retried.
	_, _ = d.Service.HandleWatchNotification(r.Context(), channelID, resourceID)
	w.WriteHeader(http.StatusOK)
}
