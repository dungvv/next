package calendar

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Outbound ports (domain::ports)
// ---------------------------------------------------------------------------

// TokenError mirrors CalendarTokenError.
type TokenError struct {
	// ReauthRequired is true when the grant is invalid/revoked/missing the
	// calendar capability (maps to CalendarMutationError::ReauthRequired).
	ReauthRequired bool
	Message        string
}

func (e *TokenError) Error() string { return e.Message }

// AccessTokenProvider mirrors CalendarAccessTokenProvider: mint or reuse an
// access token for a connected inbox.
type AccessTokenProvider interface {
	FetchAccessToken(ctx context.Context, identity CalendarLinkTokenIdentity) (string, error)
}

// InstanceUpdateOutcome mirrors GoogleInstanceUpdateOutcome.
type InstanceUpdateOutcome struct {
	// Applied / OccurrenceGone carry the refreshed series echo; GoneKind
	// selects which.
	Applied, OccurrenceGone *CalendarEventUpsert
	// SeriesGone means the provider no longer has the series.
	SeriesGone bool
}

// SeriesMutationOutcome mirrors GoogleSeriesMutationOutcome.
type SeriesMutationOutcome struct {
	Applied       *CalendarEventUpsert
	SeriesDeleted bool
	Gone          bool
}

// RsvpOutcome mirrors GoogleRsvpOutcome.
type RsvpOutcome struct {
	Applied     *CalendarEventUpsert
	NotAttendee bool
	Gone        bool
}

// GoogleMutationProvider mirrors GoogleCalendarMutationProvider: provider
// writes used by user-initiated mutations.
type GoogleMutationProvider interface {
	CreateEvent(ctx context.Context, accessToken string, target GoogleCalendarTarget, draft CalendarEventDraft) (*CalendarEventUpsert, error)
	UpdateEvent(ctx context.Context, accessToken string, target GoogleCalendarTarget, providerEventID string, patch CalendarEventPatch) (*CalendarEventUpsert, error)
	UpdateEventInstance(ctx context.Context, accessToken string, target GoogleCalendarTarget, masterProviderEventID, originalStart string, patch CalendarEventPatch) (*InstanceUpdateOutcome, error)
	DeleteEvent(ctx context.Context, accessToken string, target GoogleCalendarTarget, providerEventID string) error
	DeleteEventInstance(ctx context.Context, accessToken string, target GoogleCalendarTarget, masterProviderEventID, originalStart string) (*SeriesMutationOutcome, error)
	TruncateRecurringEvent(ctx context.Context, accessToken string, target GoogleCalendarTarget, masterProviderEventID, originalStart string) (*SeriesMutationOutcome, error)
	RsvpEvent(ctx context.Context, accessToken string, target GoogleCalendarTarget, masterProviderEventID string, actor *ActorInboxes, response AttendeeResponseStatus, scope RsvpScope) (*RsvpOutcome, error)
	StopWatchChannel(ctx context.Context, accessToken string, emailLinkID uuid.UUID, channelID, resourceID string) error
}

// GoogleSyncProvider mirrors GoogleCalendarProvider: the backfill-side reads
// (calendar list, event sync, watch channel creation). TODO(google): port
// crates/calendar_events::outbound::google.
type GoogleSyncProvider interface {
	ListCalendars(ctx context.Context, accessToken string, emailLinkID uuid.UUID) ([]ProviderCalendar, error)
	SyncEvents(ctx context.Context, accessToken string, ctx2 GoogleEventSyncContext) (*GoogleEventSyncBatch, error)
	WatchCalendar(ctx context.Context, accessToken string, emailLinkID uuid.UUID, providerCalendarID string, channelID uuid.UUID, cfg GoogleWatchConfig) (*GoogleWatchChannel, error)
}

// ProviderCalendar is one calendar returned by the provider's calendar list.
type ProviderCalendar struct {
	ProviderCalendarID string
	Name               string
	Description        *string
	TimeZone           *string
	Color              *string
	AccessRole         *string
	IsPrimary          bool
	IsSelected         bool
	DefaultReminders   []EventReminderOverride
}

// GoogleEventSyncContext mirrors domain::ports::GoogleEventSyncContext.
type GoogleEventSyncContext struct {
	Target    GoogleCalendarTarget
	SyncToken *string
	Plan      GoogleSyncPlan
}

// GoogleSyncPlan mirrors domain::models::GoogleSyncPlan.
type GoogleSyncPlan struct {
	Kind string // "full" | "extend_tail" | "incremental"
	From *OccurrenceRange
}

// GoogleEventSyncBatch mirrors domain::models::GoogleEventSyncBatch.
type GoogleEventSyncBatch struct {
	Upserts                   []CalendarEventUpsert
	ObservedProviderEventIDs  *[]string
	NextSyncToken             string
	MaterializedRange         *OccurrenceRange
	CancelledProviderEventIDs []string
}

// GoogleWatchChannel mirrors domain::models::GoogleWatchChannel.
type GoogleWatchChannel struct {
	ChannelID  uuid.UUID
	ResourceID string
	ExpiresAt  time.Time
}

// RefreshNotifier mirrors CalendarRefreshNotifier: nudges a link's calendar
// viewers to refetch after a Macro-originated mutation.
type RefreshNotifier interface {
	CalendarChanged(ctx context.Context, ownerID string, emailLinkID uuid.UUID)
}

// EventPublisher mirrors the MacroEventBroker use in mutations: announces
// calendar topic events (calendar_event.created/updated/deleted).
type EventPublisher interface {
	PublishCalendarEvent(ctx context.Context, kind string, eventID uuid.UUID, ownerID string)
}

// ---------------------------------------------------------------------------
// Stubs — TODO(google): replace with a real Google Calendar adapter once the
// OAuth token vault port (was DynamoDB gmail_tokens via authentication_service)
// lands. The stubs keep every mutation endpoint reachable and fail closed as
// `retryable`, exactly how a provider outage presents.
// ---------------------------------------------------------------------------

// ErrProviderNotPorted is returned by stub providers.
var ErrProviderNotPorted = errors.New("google calendar provider not yet ported")

// StubGoogleProvider returns ProviderError{Transient} for every call so
// mutations surface as HTTP 503 `retryable` rather than pretending success.
type StubGoogleProvider struct{}

func (StubGoogleProvider) CreateEvent(context.Context, string, GoogleCalendarTarget, CalendarEventDraft) (*CalendarEventUpsert, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) UpdateEvent(context.Context, string, GoogleCalendarTarget, string, CalendarEventPatch) (*CalendarEventUpsert, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) UpdateEventInstance(context.Context, string, GoogleCalendarTarget, string, string, CalendarEventPatch) (*InstanceUpdateOutcome, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) DeleteEvent(context.Context, string, GoogleCalendarTarget, string) error {
	return &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) DeleteEventInstance(context.Context, string, GoogleCalendarTarget, string, string) (*SeriesMutationOutcome, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) TruncateRecurringEvent(context.Context, string, GoogleCalendarTarget, string, string) (*SeriesMutationOutcome, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) RsvpEvent(context.Context, string, GoogleCalendarTarget, string, *ActorInboxes, AttendeeResponseStatus, RsvpScope) (*RsvpOutcome, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) StopWatchChannel(context.Context, string, uuid.UUID, string, string) error {
	return &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) ListCalendars(context.Context, string, uuid.UUID) ([]ProviderCalendar, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) SyncEvents(context.Context, string, GoogleEventSyncContext) (*GoogleEventSyncBatch, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}
func (StubGoogleProvider) WatchCalendar(context.Context, string, uuid.UUID, string, uuid.UUID, GoogleWatchConfig) (*GoogleWatchChannel, error) {
	return nil, &ProviderError{Kind: ProviderTransient, Message: ErrProviderNotPorted.Error()}
}

// StubTokenProvider returns TokenError{ReauthRequired} for every fetch: with
// no token vault ported, every mutation fails closed telling the client to
// reauthorize rather than attempting a provider call with no token.
//
// TODO(token-vault): port the DynamoDB gmail_tokens store (refresh-token
// ciphertext + KMS envelope) to Postgres, cache access tokens in Valkey like
// google_token::fetch_gmail_access_token did through Redis.
type StubTokenProvider struct{}

func (StubTokenProvider) FetchAccessToken(context.Context, CalendarLinkTokenIdentity) (string, error) {
	return "", &TokenError{ReauthRequired: true, Message: "google token vault not yet ported"}
}

// NoopRefreshNotifier drops refresh nudges (used when NATS is absent).
type NoopRefreshNotifier struct{}

func (NoopRefreshNotifier) CalendarChanged(context.Context, string, uuid.UUID) {}

// NoopPublisher drops topic events.
type NoopPublisher struct{}

func (NoopPublisher) PublishCalendarEvent(context.Context, string, uuid.UUID, string) {}
