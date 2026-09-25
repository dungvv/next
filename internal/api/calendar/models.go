// Package calendar ports services/calendar_service + crates/calendar_events:
// calendar event reads over the Postgres projection, user-initiated mutations
// that write through to the provider (stubbed), and the Google watch webhook.
package calendar

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Enums (DB representations match the CHECK constraints in the migrations).
// ---------------------------------------------------------------------------

// EventStatus mirrors domain::models::EventStatus.
type EventStatus string

const (
	EventStatusConfirmed EventStatus = "confirmed"
	EventStatusTentative EventStatus = "tentative"
	EventStatusCancelled EventStatus = "cancelled"
)

// EventVisibility mirrors domain::models::EventVisibility.
type EventVisibility string

const (
	EventVisibilityDefault      EventVisibility = "default"
	EventVisibilityPublic       EventVisibility = "public"
	EventVisibilityPrivate      EventVisibility = "private"
	EventVisibilityConfidential EventVisibility = "confidential"
)

// EventTransparency mirrors domain::models::EventTransparency.
type EventTransparency string

const (
	EventTransparencyOpaque      EventTransparency = "opaque"
	EventTransparencyTransparent EventTransparency = "transparent"
)

// EventType mirrors domain::models::EventType.
type EventType string

const (
	EventTypeDefault         EventType = "default"
	EventTypeOutOfOffice     EventType = "out_of_office"
	EventTypeFocusTime       EventType = "focus_time"
	EventTypeWorkingLocation EventType = "working_location"
	EventTypeBirthday        EventType = "birthday"
	EventTypeFromGmail       EventType = "from_gmail"
)

// UsesCalendarDefaultReminders mirrors EventType::uses_calendar_default_reminders.
func (t EventType) UsesCalendarDefaultReminders() bool {
	return t == EventTypeDefault || t == EventTypeFromGmail
}

// ConferenceProvider mirrors domain::models::ConferenceProvider.
type ConferenceProvider string

const (
	ConferenceProviderGoogleMeet ConferenceProvider = "google_meet"
	ConferenceProviderOther      ConferenceProvider = "other"
)

// ConferenceChange mirrors domain::models::ConferenceChange.
type ConferenceChange string

const (
	ConferenceChangeGoogleMeet ConferenceChange = "google_meet"
	ConferenceChangeRemoved    ConferenceChange = "none"
)

// AttendeeResponseStatus mirrors domain::models::AttendeeResponseStatus.
type AttendeeResponseStatus string

const (
	AttendeeNeedsAction AttendeeResponseStatus = "needs_action"
	AttendeeAccepted    AttendeeResponseStatus = "accepted"
	AttendeeDeclined    AttendeeResponseStatus = "declined"
	AttendeeTentative   AttendeeResponseStatus = "tentative"
)

// OutOfOfficeAutoDeclineMode mirrors domain::models::OutOfOfficeAutoDeclineMode.
type OutOfOfficeAutoDeclineMode string

const (
	DeclineNone                          OutOfOfficeAutoDeclineMode = "decline_none"
	DeclineAllConflictingInvitations     OutOfOfficeAutoDeclineMode = "decline_all_conflicting_invitations"
	DeclineOnlyNewConflictingInvitations OutOfOfficeAutoDeclineMode = "decline_only_new_conflicting_invitations"
)

// CalendarSyncStatus mirrors domain::models::CalendarSyncStatus.
type CalendarSyncStatus string

const (
	SyncStatusSyncing CalendarSyncStatus = "syncing"
	SyncStatusReady   CalendarSyncStatus = "ready"
)

// ---------------------------------------------------------------------------
// EventTime — tagged union {"kind":"timed"|"allDay", ...} matching serde's
// internally-tagged representation of domain::models::EventTime.
// ---------------------------------------------------------------------------

// Date is an RFC 3339 date (YYYY-MM-DD) with no zone, like chrono::NaiveDate.
type Date struct{ time.Time }

// NewDate builds a Date from components.
func NewDate(y int, m time.Month, d int) Date { return Date{time.Date(y, m, d, 0, 0, 0, 0, time.UTC)} }

// ParseDate parses YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q", s)
	}
	return Date{t}, nil
}

func (d Date) String() string { return d.Format("2006-01-02") }

// After reports strict ordering.
func (d Date) After(o Date) bool { return d.Time.After(o.Time) }

func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	t, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = t
	return nil
}

// EventTime is the mutually exclusive time shape of a calendar event.
// Exactly one of Timed/AllDay is set.
type EventTime struct {
	// Timed fields.
	StartsAt time.Time
	EndsAt   time.Time
	TimeZone *string
	// AllDay fields (exclusive end date, RFC 5545).
	StartDate Date
	EndDate   Date
	// IsAllDay selects the variant.
	IsAllDay bool
}

// NewTimedEventTime builds a Timed EventTime.
func NewTimedEventTime(start, end time.Time, tz *string) EventTime {
	return EventTime{StartsAt: start, EndsAt: end, TimeZone: tz}
}

// NewAllDayEventTime builds an AllDay EventTime with an exclusive end date.
func NewAllDayEventTime(start, end Date) EventTime {
	return EventTime{StartDate: start, EndDate: end, IsAllDay: true}
}

// IsValid mirrors EventTime::is_valid (exclusive end strictly after start).
func (t EventTime) IsValid() bool {
	if t.IsAllDay {
		return t.EndDate.After(t.StartDate)
	}
	return t.EndsAt.After(t.StartsAt)
}

// OccurrenceKey mirrors EventTime::occurrence_key.
func (t EventTime) OccurrenceKey() string {
	if t.IsAllDay {
		return t.StartDate.String()
	}
	return t.StartsAt.UTC().Format(time.RFC3339Nano)
}

// StartInstant normalizes the span's start to an instant (midnight UTC for
// all-day), matching COALESCE(starts_at, start_date::timestamp AT TIME ZONE 'UTC').
func (t EventTime) StartInstant() time.Time {
	if t.IsAllDay {
		return t.StartDate.Time
	}
	return t.StartsAt
}

type eventTimeJSON struct {
	Kind      string     `json:"kind"`
	StartsAt  *time.Time `json:"startsAt,omitempty"`
	EndsAt    *time.Time `json:"endsAt,omitempty"`
	TimeZone  *string    `json:"timeZone,omitempty"`
	StartDate *Date      `json:"startDate,omitempty"`
	EndDate   *Date      `json:"endDate,omitempty"`
}

// MarshalJSON emits serde's tagged shape: {"kind":"timed",...} / {"kind":"allDay",...}.
func (t EventTime) MarshalJSON() ([]byte, error) {
	j := eventTimeJSON{TimeZone: t.TimeZone}
	if t.IsAllDay {
		j.Kind = "allDay"
		sd, ed := t.StartDate, t.EndDate
		j.StartDate, j.EndDate = &sd, &ed
	} else {
		j.Kind = "timed"
		s, e := t.StartsAt, t.EndsAt
		j.StartsAt, j.EndsAt = &s, &e
	}
	return json.Marshal(j)
}

// UnmarshalJSON accepts the tagged shape.
func (t *EventTime) UnmarshalJSON(b []byte) error {
	var j eventTimeJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	switch j.Kind {
	case "timed":
		if j.StartsAt == nil || j.EndsAt == nil {
			return fmt.Errorf("timed event requires startsAt and endsAt")
		}
		*t = EventTime{StartsAt: *j.StartsAt, EndsAt: *j.EndsAt, TimeZone: j.TimeZone}
	case "allDay":
		if j.StartDate == nil || j.EndDate == nil {
			return fmt.Errorf("all-day event requires startDate and endDate")
		}
		*t = EventTime{IsAllDay: true, StartDate: *j.StartDate, EndDate: *j.EndDate}
	default:
		return fmt.Errorf("unknown event time kind %q", j.Kind)
	}
	return nil
}

// EventStart identifies an overridden occurrence by its original start.
type EventStart struct {
	StartsAt  time.Time
	StartDate Date
	IsAllDay  bool
}

func (s EventStart) MarshalJSON() ([]byte, error) {
	if s.IsAllDay {
		return json.Marshal(map[string]any{"allDay": s.StartDate})
	}
	return json.Marshal(map[string]any{"timed": s.StartsAt})
}

func (s *EventStart) UnmarshalJSON(b []byte) error {
	var j map[string]json.RawMessage
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	if raw, ok := j["allDay"]; ok {
		var d Date
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		*s = EventStart{StartDate: d, IsAllDay: true}
		return nil
	}
	if raw, ok := j["timed"]; ok {
		var t time.Time
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		*s = EventStart{StartsAt: t}
		return nil
	}
	return fmt.Errorf("unknown event start shape")
}

// ---------------------------------------------------------------------------
// Reminders
// ---------------------------------------------------------------------------

const (
	// ReminderMethodPopup fires a Macro notification.
	ReminderMethodPopup = "popup"
	// ReminderMethodEmail is delivered by Google itself.
	ReminderMethodEmail = "email"
	// ReminderMinutesMax is Google's four-week reminder cap.
	ReminderMinutesMax = 40320
	// ReminderOverridesMax is Google's five-reminder cap.
	ReminderOverridesMax = 5
)

// EventReminderOverride mirrors domain::models::EventReminderOverride.
type EventReminderOverride struct {
	Method  string `json:"method"`
	Minutes uint32 `json:"minutes"`
}

// EventReminders mirrors domain::models::EventReminders.
type EventReminders struct {
	UseDefault bool                    `json:"useDefault"`
	Overrides  []EventReminderOverride `json:"overrides,omitempty"`
}

// IsDefault mirrors EventReminders::is_default.
func (r EventReminders) IsDefault() bool { return r.UseDefault && len(r.Overrides) == 0 }

// PopupMinutes resolves deduped, sorted popup offsets (calendar defaults apply
// when useDefault is on).
func (r EventReminders) PopupMinutes(calendarDefaults []EventReminderOverride) []uint32 {
	overrides := r.Overrides
	if r.UseDefault {
		overrides = calendarDefaults
	}
	minutes := make([]uint32, 0, len(overrides))
	for _, o := range overrides {
		if o.Method == ReminderMethodPopup {
			minutes = append(minutes, o.Minutes)
		}
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	// dedupe
	out := minutes[:0]
	for _, m := range minutes {
		if len(out) == 0 || out[len(out)-1] != m {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Event content
// ---------------------------------------------------------------------------

// CalendarAttendee mirrors domain::models::CalendarAttendee.
type CalendarAttendee struct {
	Email          string                 `json:"email"`
	DisplayName    *string                `json:"displayName,omitempty"`
	ResponseStatus AttendeeResponseStatus `json:"responseStatus"`
	IsOrganizer    bool                   `json:"isOrganizer"`
	IsOptional     bool                   `json:"isOptional"`
	IsSelf         bool                   `json:"isSelf"`
	Comment        *string                `json:"comment,omitempty"`
}

// CalendarAttendeeInput is an attendee supplied to a mutation.
type CalendarAttendeeInput struct {
	Email          string
	IsOptional     bool
	ResponseStatus *AttendeeResponseStatus
}

// OutOfOfficeProperties mirrors domain::models::OutOfOfficeProperties.
type OutOfOfficeProperties struct {
	AutoDeclineMode OutOfOfficeAutoDeclineMode `json:"autoDeclineMode"`
	DeclineMessage  *string                    `json:"declineMessage,omitempty"`
}

// CalendarEventSourceContent mirrors domain::models::CalendarEventSourceContent.
type CalendarEventSourceContent struct {
	CalendarID   uuid.UUID         `json:"calendarId"`
	Title        string            `json:"title"`
	Description  *string           `json:"description,omitempty"`
	Location     *string           `json:"location,omitempty"`
	EventType    EventType         `json:"eventType"`
	Visibility   EventVisibility   `json:"visibility"`
	Transparency EventTransparency `json:"transparency"`
	IsReadOnly   bool              `json:"isReadOnly"`
	Reminders    EventReminders    `json:"reminders"`
	CreatorEmail *string           `json:"creatorEmail,omitempty"`
	CreatorName  *string           `json:"creatorName,omitempty"`
}

// CalendarEvent mirrors domain::models::CalendarEvent (camelCase JSON).
type CalendarEvent struct {
	ID                 uuid.UUID                    `json:"id"`
	OwnerID            string                       `json:"ownerId"`
	ICalUID            string                       `json:"icalUid"`
	CalendarID         *uuid.UUID                   `json:"calendarId,omitempty"`
	Sources            []CalendarEventSourceContent `json:"sources,omitempty"`
	Title              string                       `json:"title"`
	Description        *string                      `json:"description,omitempty"`
	Location           *string                      `json:"location,omitempty"`
	Status             EventStatus                  `json:"status"`
	Visibility         EventVisibility              `json:"visibility"`
	Transparency       EventTransparency            `json:"transparency"`
	EventType          EventType                    `json:"eventType,omitempty"`
	Time               EventTime                    `json:"time"`
	RecurrenceLines    []string                     `json:"recurrenceLines"`
	OrganizerEmail     *string                      `json:"organizerEmail,omitempty"`
	OrganizerName      *string                      `json:"organizerName,omitempty"`
	CreatorEmail       *string                      `json:"creatorEmail,omitempty"`
	CreatorName        *string                      `json:"creatorName,omitempty"`
	ConferenceURL      *string                      `json:"conferenceUrl,omitempty"`
	ConferenceProvider *ConferenceProvider          `json:"conferenceProvider,omitempty"`
	Sequence           uint32                       `json:"sequence"`
	IsReadOnly         bool                         `json:"isReadOnly"`
	Attendees          []CalendarAttendee           `json:"attendees"`
	Reminders          EventReminders               `json:"reminders,omitempty"`
	CreatedAt          time.Time                    `json:"createdAt"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
}

// ApplyOccurrenceContent mirrors CalendarEvent::apply_occurrence_content.
func (e *CalendarEvent) ApplyOccurrenceContent(title, description, location *string, status *EventStatus) {
	if title != nil {
		e.Title = *title
		for i := range e.Sources {
			e.Sources[i].Title = *title
		}
	}
	if description != nil {
		e.Description = description
		for i := range e.Sources {
			e.Sources[i].Description = description
		}
	}
	if location != nil {
		e.Location = location
		for i := range e.Sources {
			e.Sources[i].Location = location
		}
	}
	if status != nil {
		e.Status = *status
	}
}

// CalendarEventOverride mirrors domain::models::CalendarEventOverride.
type CalendarEventOverride struct {
	RecurrenceID string              `json:"recurrenceId"`
	OriginalTime EventStart          `json:"originalTime"`
	Time         EventTime           `json:"time"`
	Title        *string             `json:"title,omitempty"`
	Description  *string             `json:"description,omitempty"`
	Location     *string             `json:"location,omitempty"`
	Status       *EventStatus        `json:"status,omitempty"`
	Attendees    *[]CalendarAttendee `json:"attendees,omitempty"`
}

// ApplyTo mirrors CalendarEventOverride::apply_to.
func (o *CalendarEventOverride) ApplyTo(e *CalendarEvent) {
	e.ApplyOccurrenceContent(o.Title, o.Description, o.Location, o.Status)
	e.Time = o.Time
	if o.Attendees != nil {
		e.Attendees = *o.Attendees
	}
}

// CalendarOccurrence mirrors domain::models::CalendarOccurrence.
type CalendarOccurrence struct {
	EventID       uuid.UUID `json:"eventId"`
	OccurrenceKey string    `json:"occurrenceKey"`
	RecurrenceID  *string   `json:"recurrenceId,omitempty"`
	Time          EventTime `json:"time"`
	IsCancelled   bool      `json:"isCancelled"`
}

// CalendarOccurrenceCursor mirrors domain::models::CalendarOccurrenceCursor.
type CalendarOccurrenceCursor struct {
	StartsAt      time.Time `json:"startsAt"`
	EventID       uuid.UUID `json:"eventId"`
	OccurrenceKey string    `json:"occurrenceKey"`
}

// CursorFromOccurrence mirrors CalendarOccurrenceCursor::from_occurrence.
func CursorFromOccurrence(o *CalendarOccurrence) CalendarOccurrenceCursor {
	return CalendarOccurrenceCursor{
		StartsAt:      o.Time.StartInstant(),
		EventID:       o.EventID,
		OccurrenceKey: o.OccurrenceKey,
	}
}

// Encode encodes the cursor like Base64Str::encode_json.
func (c CalendarOccurrenceCursor) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor parses an opaque cursor.
func DecodeCursor(s string) (*CalendarOccurrenceCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// tolerate padded variants from older clients
		raw, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			raw, err = base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, fmt.Errorf("invalid cursor")
			}
		}
	}
	var c CalendarOccurrenceCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &c, nil
}

// OccurrenceRange mirrors domain::models::OccurrenceRange.
type OccurrenceRange struct {
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	StartDate Date      `json:"startDate"`
	EndDate   Date      `json:"endDate"`
}

// IsValid mirrors OccurrenceRange::is_valid.
func (r OccurrenceRange) IsValid() bool {
	return r.EndsAt.After(r.StartsAt) &&
		r.EndDate.After(r.StartDate) &&
		r.EndsAt.Sub(r.StartsAt) <= 370*24*time.Hour &&
		r.EndDate.Time.Sub(r.StartDate.Time) <= 370*24*time.Hour
}

// IsMaterializedAt mirrors OccurrenceRange::is_materialized_at.
func (r OccurrenceRange) IsMaterializedAt(now time.Time) bool {
	materialized := HistoricalSyncRange(now)
	return !r.StartsAt.Before(materialized.StartsAt) &&
		!r.EndsAt.After(materialized.EndsAt) &&
		!r.StartDate.Time.Before(materialized.StartDate.Time) &&
		!r.EndDate.Time.After(materialized.EndDate.Time)
}

// HistoricalSyncRange mirrors OccurrenceRange::historical_sync (now-365d..now+730d).
func HistoricalSyncRange(now time.Time) OccurrenceRange {
	starts := now.AddDate(0, 0, -365)
	ends := now.AddDate(0, 0, 730)
	return OccurrenceRange{
		StartsAt:  starts,
		EndsAt:    ends,
		StartDate: Date{time.Date(starts.Year(), starts.Month(), starts.Day(), 0, 0, 0, 0, time.UTC)},
		EndDate:   Date{time.Date(ends.Year(), ends.Month(), ends.Day(), 0, 0, 0, 0, time.UTC)},
	}
}

// MaintenanceHorizon mirrors OccurrenceRange::maintenance_horizon.
func MaintenanceHorizon(now time.Time) OccurrenceRange {
	starts := now.AddDate(0, 0, -365)
	ends := monthCeil(now.AddDate(0, 0, 730))
	return OccurrenceRange{
		StartsAt:  starts,
		EndsAt:    ends,
		StartDate: Date{time.Date(starts.Year(), starts.Month(), starts.Day(), 0, 0, 0, 0, time.UTC)},
		EndDate:   Date{time.Date(ends.Year(), ends.Month(), ends.Day(), 0, 0, 0, 0, time.UTC)},
	}
}

func monthCeil(t time.Time) time.Time {
	y, m := t.Year(), t.Month()
	if m == time.December {
		y, m = y+1, time.January
	} else {
		m++
	}
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Read-side projections
// ---------------------------------------------------------------------------

// TeamOutOfOffice mirrors domain::models::TeamOutOfOffice.
type TeamOutOfOffice struct {
	OwnerID       string
	EventID       uuid.UUID
	ICalUID       string
	OccurrenceKey string
	Title         *string
	Visibility    EventVisibility
	Time          EventTime
}

// CalendarMentionRequestItem mirrors domain::models::CalendarMentionRequestItem.
type CalendarMentionRequestItem struct {
	EventID       uuid.UUID
	OccurrenceKey *string
}

// CalendarMentionEvent mirrors domain::models::CalendarMentionEvent.
type CalendarMentionEvent struct {
	ViewerEventID  uuid.UUID `json:"viewerEventId"`
	Title          string    `json:"title"`
	Time           EventTime `json:"time"`
	OccurrenceKey  *string   `json:"occurrenceKey,omitempty"`
	IsRecurring    bool      `json:"isRecurring"`
	Location       *string   `json:"location,omitempty"`
	OrganizerEmail *string   `json:"organizerEmail,omitempty"`
	OrganizerName  *string   `json:"organizerName,omitempty"`
	AttendeeCount  int       `json:"attendeeCount"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// CalendarMentionPreview mirrors domain::models::CalendarMentionPreview.
type CalendarMentionPreview struct {
	// Kind is "accessible", "noAccess", or "doesNotExist" — mapped to the
	// transport's snake_case enum by the handler.
	Kind  MentionPreviewKind
	Event *CalendarMentionEvent
}

// MentionPreviewKind is the requester-relative visibility of a mentioned event.
type MentionPreviewKind int

const (
	MentionAccessible MentionPreviewKind = iota
	MentionNoAccess
	MentionDoesNotExist
)

// VisibleCalendar mirrors domain::models::VisibleCalendar.
type VisibleCalendar struct {
	ID               uuid.UUID               `json:"id"`
	EmailLinkID      uuid.UUID               `json:"emailLinkId"`
	EmailAddress     string                  `json:"emailAddress"`
	Name             string                  `json:"name"`
	Color            *string                 `json:"color,omitempty"`
	IsPrimary        bool                    `json:"isPrimary"`
	IsWritable       bool                    `json:"isWritable"`
	IsSubscription   bool                    `json:"isSubscription"`
	SyncError        *string                 `json:"syncError,omitempty"`
	DefaultReminders []EventReminderOverride `json:"defaultReminders"`
}

// ---------------------------------------------------------------------------
// Mutation inputs
// ---------------------------------------------------------------------------

// CalendarEventDraft mirrors domain::models::CalendarEventDraft.
type CalendarEventDraft struct {
	Title           string
	Description     *string
	Location        *string
	Time            EventTime
	Attendees       []CalendarAttendeeInput
	RecurrenceLines []string
	Visibility      *EventVisibility
	Transparency    *EventTransparency
	Reminders       *EventReminders
	Conference      *ConferenceChange
	OutOfOffice     *OutOfOfficeProperties
}

// CalendarEventPatch mirrors domain::models::CalendarEventPatch. Pointer
// fields distinguish "omitted" (nil, untouched) from "set" — including set
// to an empty string/list, which clears the value at the provider.
type CalendarEventPatch struct {
	Title           *string
	Description     *string
	Location        *string
	Time            *EventTime
	Attendees       *[]CalendarAttendeeInput
	RecurrenceLines *[]string
	Visibility      *EventVisibility
	Transparency    *EventTransparency
	Reminders       *EventReminders
	Conference      *ConferenceChange
	OutOfOffice     *OutOfOfficeProperties
}

// IsEmpty mirrors CalendarEventPatch::is_empty.
func (p *CalendarEventPatch) IsEmpty() bool {
	return p.Title == nil && p.Description == nil && p.Location == nil &&
		p.Time == nil && p.Attendees == nil && p.RecurrenceLines == nil &&
		p.Visibility == nil && p.Transparency == nil && p.Reminders == nil &&
		p.Conference == nil && p.OutOfOffice == nil
}

// Scopes mirror domain::ports::{CalendarUpdateScope, CalendarDeletionScope,
// CalendarRsvpScope}. A scope is either All or ThisEvent/ThisAndFollowing
// keyed by an occurrence's original start.
type (
	// UpdateScope selects how much of a series an update covers.
	UpdateScope struct {
		ThisEvent *string // recurrence_id when set, else All
	}
	// DeletionScope selects how much of a series a deletion removes.
	DeletionScope struct {
		Kind         string // "all" | "this_event" | "this_and_following"
		RecurrenceID string
	}
	// RsvpScope selects how much of a series an RSVP covers.
	RsvpScope struct {
		ThisEvent *string
	}
)

// CalendarLinkTokenIdentity mirrors domain::models::CalendarLinkTokenIdentity.
// ProviderUserID was `fusionauth_user_id`; Casdoor keeps the same column.
type CalendarLinkTokenIdentity struct {
	ProviderUserID string
	EmailAddress   string
	Provider       string
}

// ActorInboxes mirrors domain::acting::ActorInboxes.
type ActorInboxes struct {
	Emails []string
}

// ActorInboxesFromOwned mirrors ActorInboxes::from_owned.
func ActorInboxesFromOwned(owned []string) *ActorInboxes {
	emails := make([]string, 0, len(owned))
	seen := map[string]bool{}
	for _, e := range owned {
		e = strings.ToLower(e)
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		emails = append(emails, e)
	}
	sort.Strings(emails)
	if len(emails) == 0 {
		return nil
	}
	return &ActorInboxes{Emails: emails}
}

// Matches mirrors ActorInboxes::matches.
func (a *ActorInboxes) Matches(email string) bool {
	email = strings.ToLower(email)
	for _, e := range a.Emails {
		if e == email {
			return true
		}
	}
	return false
}

// MarkAttendees mirrors ActorInboxes::mark_attendees.
func (a *ActorInboxes) MarkAttendees(attendees []CalendarAttendee) {
	for i := range attendees {
		attendees[i].IsSelf = a.Matches(attendees[i].Email)
	}
}

// CalendarEventMutationTarget mirrors domain::models::CalendarEventMutationTarget.
type CalendarEventMutationTarget struct {
	EventID                  uuid.UUID
	IsReadOnly               bool
	ProviderEventID          string
	ProviderRecurringEventID *string
	OwnerID                  string
	EmailLinkID              uuid.UUID
	AccountID                uuid.UUID
	CalendarID               uuid.UUID
	ProviderCalendarID       string
	TokenIdentity            CalendarLinkTokenIdentity
	Actor                    *ActorInboxes
}

// MasterProviderEventID mirrors CalendarEventMutationTarget::master_provider_event_id.
func (t *CalendarEventMutationTarget) MasterProviderEventID() string {
	if t.ProviderRecurringEventID != nil {
		return *t.ProviderRecurringEventID
	}
	return t.ProviderEventID
}

// GoogleTarget builds the provider target for a mutation window.
func (t *CalendarEventMutationTarget) GoogleTarget(r OccurrenceRange) GoogleCalendarTarget {
	return GoogleCalendarTarget{
		OwnerID:            t.OwnerID,
		EmailLinkID:        t.EmailLinkID,
		AccountID:          t.AccountID,
		CalendarID:         t.CalendarID,
		ProviderCalendarID: t.ProviderCalendarID,
		IsReadOnly:         t.IsReadOnly,
		Range:              r,
	}
}

// CalendarCreationTarget mirrors domain::models::CalendarCreationTarget.
type CalendarCreationTarget struct {
	OwnerID            string
	EmailLinkID        uuid.UUID
	AccountID          uuid.UUID
	CalendarID         uuid.UUID
	ProviderCalendarID string
	IsReadOnly         bool
	IsPrimary          bool
	TokenIdentity      CalendarLinkTokenIdentity
	Actor              *ActorInboxes
}

// GoogleTarget builds the provider target for a creation window.
func (t *CalendarCreationTarget) GoogleTarget(r OccurrenceRange) GoogleCalendarTarget {
	return GoogleCalendarTarget{
		OwnerID:            t.OwnerID,
		EmailLinkID:        t.EmailLinkID,
		AccountID:          t.AccountID,
		CalendarID:         t.CalendarID,
		ProviderCalendarID: t.ProviderCalendarID,
		IsReadOnly:         t.IsReadOnly,
		Range:              r,
	}
}

// GoogleCalendarTarget mirrors domain::models::GoogleCalendarTarget.
type GoogleCalendarTarget struct {
	OwnerID            string
	EmailLinkID        uuid.UUID
	AccountID          uuid.UUID
	CalendarID         uuid.UUID
	ProviderCalendarID string
	IsReadOnly         bool
	Range              OccurrenceRange
}

// GoogleEventSource mirrors domain::models::GoogleEventSource.
type GoogleEventSource struct {
	EmailLinkID              uuid.UUID
	AccountID                uuid.UUID
	CalendarID               uuid.UUID
	ProviderEventID          string
	ProviderRecurringEventID *string
	ProviderETag             *string
	RawPayload               json.RawMessage
}

// CalendarEventUpsert mirrors domain::models::CalendarEventUpsert.
type CalendarEventUpsert struct {
	Event       CalendarEvent
	Source      GoogleEventSource
	Overrides   []CalendarEventOverride
	Occurrences []CalendarOccurrence
}

// CalendarEventChange mirrors domain::ports::CalendarEventChange.
type CalendarEventChange string

const (
	ChangeCreated   CalendarEventChange = "created"
	ChangeUpdated   CalendarEventChange = "updated"
	ChangeUnchanged CalendarEventChange = "unchanged"
)

// CalendarEventWriteOutcome mirrors domain::ports::CalendarEventWriteOutcome.
type CalendarEventWriteOutcome struct {
	EventID uuid.UUID
	OwnerID string
	Change  CalendarEventChange
}

// RetiredCalendarEvent mirrors domain::ports::RetiredCalendarEvent.
type RetiredCalendarEvent struct {
	EventID uuid.UUID
	OwnerID string
	Deleted bool
}

// StoredSourceProjection is the per-copy projection persisted as
// calendar_event_sources.normalized_payload (serde camelCase).
type StoredSourceProjection struct {
	Event       CalendarEvent           `json:"event"`
	Overrides   []CalendarEventOverride `json:"overrides"`
	Occurrences []CalendarOccurrence    `json:"occurrences"`
}

// GoogleWatchConfig mirrors domain::models::GoogleWatchConfig.
type GoogleWatchConfig struct {
	Address string
	Token   string
}

// IsSystemCalendar mirrors domain::models::is_system_calendar.
func IsSystemCalendar(providerCalendarID string) bool {
	return strings.HasSuffix(providerCalendarID, "@group.v.calendar.google.com")
}

// CalendarSyncFailureBadgeThreshold mirrors CALENDAR_SYNC_FAILURE_BADGE_THRESHOLD.
const CalendarSyncFailureBadgeThreshold = 3

// ConstantTimeTokenEqual compares webhook tokens in constant time.
func ConstantTimeTokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// CalendarEventMessage mirrors CalendarEventPubSub's wire payload
// (calendar topic: {kind, event_id, owner_id}).
type CalendarEventMessage struct {
	Kind    string    `json:"kind"`
	EventID uuid.UUID `json:"event_id"`
	OwnerID string    `json:"owner_id"`
}
