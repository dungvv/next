package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/macro-inc/macro/pkg/events"
)

// Service is the combined calendar service: occurrence reads
// (CalendarOccurrenceService) plus user-initiated mutations
// (CalendarMutationService) and the watch-notification handler.
type Service struct {
	repo      *Repo
	provider  GoogleMutationProvider
	tokens    AccessTokenProvider
	publisher EventPublisher
	refresh   RefreshNotifier
}

// NewService builds the service from its ports.
func NewService(repo *Repo, provider GoogleMutationProvider, tokens AccessTokenProvider, pub EventPublisher, refresh RefreshNotifier) *Service {
	return &Service{repo: repo, provider: provider, tokens: tokens, publisher: pub, refresh: refresh}
}

// ---------------------------------------------------------------------------
// reads (CalendarOccurrenceService)
// ---------------------------------------------------------------------------

func validateQuery(rng OccurrenceRange, limit int) error {
	if limit < 1 || limit > 2001 {
		return errInvalidLimit
	}
	if !rng.IsValid() || !rng.IsMaterializedAt(time.Now().UTC()) {
		return errInvalidRange
	}
	return nil
}

// ListOccurrences mirrors CalendarService::list_occurrences.
func (s *Service) ListOccurrences(ctx context.Context, requesterID string, rng OccurrenceRange, cursor *CalendarOccurrenceCursor, limit int) ([]struct {
	Event      CalendarEvent
	Occurrence CalendarOccurrence
}, error) {
	if err := validateQuery(rng, limit); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListOccurrences(ctx, requesterID, rng, cursor, limit)
	if err != nil {
		return nil, err
	}
	owned, err := s.repo.OwnedInboxEmails(ctx, requesterID)
	if err != nil {
		return nil, err
	}
	if viewer := ActorInboxesFromOwned(owned); viewer != nil {
		for i := range rows {
			viewer.MarkAttendees(rows[i].Event.Attendees)
		}
	}
	return rows, nil
}

// ListTeamOutOfOffice mirrors CalendarService::list_team_out_of_office —
// private/confidential titles are withheld, matching Google's viewer policy.
func (s *Service) ListTeamOutOfOffice(ctx context.Context, requesterID string, rng OccurrenceRange, limit int) ([]TeamOutOfOffice, error) {
	if err := validateQuery(rng, limit); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListTeamOutOfOffice(ctx, requesterID, rng, limit)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Visibility == EventVisibilityPrivate || rows[i].Visibility == EventVisibilityConfidential {
			rows[i].Title = nil
		}
	}
	return rows, nil
}

// SyncStatus mirrors CalendarService::sync_status.
func (s *Service) SyncStatus(ctx context.Context, requesterID string) (CalendarSyncStatus, error) {
	return s.repo.SyncStatus(ctx, requesterID)
}

// MentionPreviews mirrors CalendarService::mention_previews.
func (s *Service) MentionPreviews(ctx context.Context, requesterID string, items []CalendarMentionRequestItem) ([]CalendarMentionPreview, error) {
	if len(items) > MentionPreviewsMax {
		return nil, errTooMany
	}
	if len(items) == 0 {
		return nil, nil
	}
	return s.repo.MentionPreviews(ctx, requesterID, items, time.Now().UTC())
}

// PrimaryTimeZone mirrors CalendarService::primary_time_zone.
func (s *Service) PrimaryTimeZone(ctx context.Context, requesterID string) (*string, error) {
	return s.repo.PrimaryTimeZone(ctx, requesterID)
}

// HandleWatchNotification mirrors CalendarService::handle_watch_notification:
// resolve the channel to its inbox, re-arm the sync job; the periodic poll is
// the backstop for unmatched notifications.
func (s *Service) HandleWatchNotification(ctx context.Context, channelID, resourceID string) (bool, error) {
	linkID, err := s.repo.FindWatchTarget(ctx, channelID, resourceID)
	if err != nil {
		return false, err
	}
	if linkID == nil {
		return false, nil
	}
	if _, err := s.repo.ScheduleGoogleSyncForLink(ctx, *linkID); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// mutations (CalendarMutationServiceImpl)
// ---------------------------------------------------------------------------

func internalErr(err error) error {
	return &MutationError{ErrCodeRetryable, fmt.Sprintf("calendar mutation failed transiently: %v", err)}
}

func (s *Service) resolveMutationTarget(ctx context.Context, requesterID string, eventID uuid.UUID, calendarID *uuid.UUID) (*CalendarEventMutationTarget, error) {
	target, err := s.repo.GetEventMutationTarget(ctx, requesterID, eventID, calendarID)
	if err != nil {
		return nil, internalErr(err)
	}
	if target == nil {
		return nil, ErrMutationNotFound
	}
	return target, nil
}

func (s *Service) fetchToken(ctx context.Context, identity CalendarLinkTokenIdentity) (string, error) {
	tok, err := s.tokens.FetchAccessToken(ctx, identity)
	if err != nil {
		var te *TokenError
		if errors.As(err, &te) {
			if te.ReauthRequired {
				return "", &MutationError{ErrCodeReauthRequired, te.Message}
			}
			return "", &MutationError{ErrCodeRetryable, te.Message}
		}
		return "", internalErr(err)
	}
	return tok, nil
}

func (s *Service) publishWriteOutcome(ctx context.Context, outcome *CalendarEventWriteOutcome) {
	switch outcome.Change {
	case ChangeCreated:
		s.publisher.PublishCalendarEvent(ctx, "created", outcome.EventID, outcome.OwnerID)
	case ChangeUpdated:
		s.publisher.PublishCalendarEvent(ctx, "updated", outcome.EventID, outcome.OwnerID)
	}
}

// persistEcho mirrors persist_echo: store the provider echo, announce the
// write, return the canonical event (with entity id applied).
func (s *Service) persistEcho(ctx context.Context, viewer *ActorInboxes, upsert CalendarEventUpsert) (CalendarEvent, error) {
	event := upsert.Event
	return s.persistEchoAs(ctx, viewer, upsert, event)
}

// persistOccurrenceEcho mirrors persist_occurrence_echo: the stored entity is
// the series master, but the response reads as the patched occurrence.
func (s *Service) persistOccurrenceEcho(ctx context.Context, viewer *ActorInboxes, upsert CalendarEventUpsert, recurrenceID string) (CalendarEvent, error) {
	event := upsert.Event
	for i := range upsert.Overrides {
		if upsert.Overrides[i].RecurrenceID == recurrenceID {
			upsert.Overrides[i].ApplyTo(&event)
			break
		}
	}
	return s.persistEchoAs(ctx, viewer, upsert, event)
}

func (s *Service) persistEchoAs(ctx context.Context, viewer *ActorInboxes, upsert CalendarEventUpsert, event CalendarEvent) (CalendarEvent, error) {
	outcome, err := s.repo.UpsertEvent(ctx, upsert, true)
	if err != nil {
		return CalendarEvent{}, &MutationError{ErrCodePersistFailed, fmt.Sprintf("calendar mutation was applied but local persistence failed: %v", err)}
	}
	event.ID = outcome.EventID
	s.publishWriteOutcome(ctx, outcome)
	if outcome.Change != ChangeUnchanged {
		s.refresh.CalendarChanged(ctx, outcome.OwnerID, upsert.Source.EmailLinkID)
	}
	if viewer != nil {
		viewer.MarkAttendees(event.Attendees)
	}
	return event, nil
}

func (s *Service) publishRetirements(ctx context.Context, retired []RetiredCalendarEvent) {
	for _, e := range retired {
		kind := "updated"
		if e.Deleted {
			kind = "deleted"
		}
		s.publisher.PublishCalendarEvent(ctx, kind, e.EventID, e.OwnerID)
	}
}

func (s *Service) announceRetirements(ctx context.Context, ownerID string, emailLinkID uuid.UUID, retired []RetiredCalendarEvent) {
	if len(retired) == 0 {
		return
	}
	s.publishRetirements(ctx, retired)
	s.refresh.CalendarChanged(ctx, ownerID, emailLinkID)
}

// retireGoneSource mirrors retire_gone_source (best-effort cleanup).
func (s *Service) retireGoneSource(ctx context.Context, target *CalendarEventMutationTarget) {
	retired, err := s.repo.RemoveGoogleSource(ctx, target.AccountID, target.CalendarID, target.MasterProviderEventID())
	if err != nil {
		slog.Warn("calendar: failed to retire a provider-deleted source", "event_id", target.EventID, "err", err)
	}
	s.announceRetirements(ctx, target.OwnerID, target.EmailLinkID, retired)
}

// CreateEvent mirrors CalendarMutationServiceImpl::create_event.
func (s *Service) CreateEvent(ctx context.Context, requesterID string, emailLinkID, calendarID *uuid.UUID, draft CalendarEventDraft) (CalendarEvent, error) {
	if err := validateTime(&draft.Time); err != nil {
		return CalendarEvent{}, err
	}
	for _, a := range draft.Attendees {
		if err := validateAttendeeEmail(a.Email); err != nil {
			return CalendarEvent{}, err
		}
	}
	if draft.Reminders != nil {
		if err := validateReminders(draft.Reminders); err != nil {
			return CalendarEvent{}, err
		}
	}
	target, err := s.repo.GetCreationTarget(ctx, requesterID, emailLinkID, calendarID)
	if err != nil {
		return CalendarEvent{}, internalErr(err)
	}
	if target == nil {
		return CalendarEvent{}, ErrNoWritableCalendar
	}
	if target.IsReadOnly {
		return CalendarEvent{}, ErrReadOnly
	}
	if draft.OutOfOffice != nil {
		if err := validateOutOfOfficeCreate(&draft, target); err != nil {
			return CalendarEvent{}, err
		}
	} else {
		ensureOrganizerAttendee(&draft.Attendees, target.TokenIdentity.EmailAddress)
	}
	accessToken, err := s.fetchToken(ctx, target.TokenIdentity)
	if err != nil {
		return CalendarEvent{}, err
	}
	upsert, err := s.provider.CreateEvent(ctx, accessToken, target.GoogleTarget(MaintenanceHorizon(time.Now().UTC())), draft)
	if err != nil {
		return CalendarEvent{}, providerMutationError(err)
	}
	return s.persistEcho(ctx, target.Actor, *upsert)
}

// UpdateEvent mirrors CalendarMutationServiceImpl::update_event.
func (s *Service) UpdateEvent(ctx context.Context, requesterID string, eventID uuid.UUID, calendarID *uuid.UUID, patch CalendarEventPatch, scope UpdateScope) (CalendarEvent, error) {
	if patch.IsEmpty() {
		return CalendarEvent{}, mutationErr(ErrCodeInvalidInput, "the patch changes nothing")
	}
	if patch.Time != nil {
		if err := validateTime(patch.Time); err != nil {
			return CalendarEvent{}, err
		}
	}
	if patch.Attendees != nil {
		for _, a := range *patch.Attendees {
			if err := validateAttendeeEmail(a.Email); err != nil {
				return CalendarEvent{}, err
			}
		}
	}
	if patch.Reminders != nil {
		if err := validateReminders(patch.Reminders); err != nil {
			return CalendarEvent{}, err
		}
	}
	if scope.ThisEvent != nil && patch.RecurrenceLines != nil {
		return CalendarEvent{}, mutationErr(ErrCodeInvalidInput,
			"a single occurrence has no recurrence of its own; update the whole series to change the recurrence")
	}
	target, err := s.resolveMutationTarget(ctx, requesterID, eventID, calendarID)
	if err != nil {
		return CalendarEvent{}, err
	}
	if target.IsReadOnly {
		return CalendarEvent{}, ErrReadOnly
	}
	if patch.Attendees != nil {
		stored, err := s.repo.GetEventAttendees(ctx, eventID)
		if err != nil {
			return CalendarEvent{}, internalErr(err)
		}
		if scope.ThisEvent != nil {
			if overrides, err := s.repo.GetOccurrenceOverrideAttendees(ctx, eventID, *scope.ThisEvent); err != nil {
				return CalendarEvent{}, internalErr(err)
			} else if overrides != nil {
				stored = append(stored, *overrides...)
			}
		}
		preserveRetainedAttendeeState(*patch.Attendees, stored)
	}
	accessToken, err := s.fetchToken(ctx, target.TokenIdentity)
	if err != nil {
		return CalendarEvent{}, err
	}
	googleTarget := target.GoogleTarget(MaintenanceHorizon(time.Now().UTC()))
	if scope.ThisEvent == nil {
		upsert, err := s.provider.UpdateEvent(ctx, accessToken, googleTarget, target.MasterProviderEventID(), patch)
		if err != nil {
			return CalendarEvent{}, providerMutationError(err)
		}
		if upsert == nil {
			// The provider no longer has the event; retire the stale local
			// source the same way a feed tombstone would.
			s.retireGoneSource(ctx, target)
			return CalendarEvent{}, ErrMutationNotFound
		}
		return s.persistEcho(ctx, target.Actor, *upsert)
	}
	outcome, err := s.provider.UpdateEventInstance(ctx, accessToken, googleTarget, target.MasterProviderEventID(), *scope.ThisEvent, patch)
	if err != nil {
		return CalendarEvent{}, providerMutationError(err)
	}
	switch {
	case outcome.Applied != nil:
		return s.persistOccurrenceEcho(ctx, target.Actor, *outcome.Applied, *scope.ThisEvent)
	case outcome.OccurrenceGone != nil:
		// Persist the provider's fresher series view so the phantom
		// occurrence disappears; the call itself still reports 404.
		if _, perr := s.persistEcho(ctx, target.Actor, *outcome.OccurrenceGone); perr != nil {
			slog.Warn("calendar: failed to persist the series refresh for a vanished occurrence",
				"event_id", target.EventID, "err", perr)
		}
		return CalendarEvent{}, ErrOccurrenceNotFound
	default: // SeriesGone
		s.retireGoneSource(ctx, target)
		return CalendarEvent{}, ErrMutationNotFound
	}
}

// DeleteEvent mirrors CalendarMutationServiceImpl::delete_event.
func (s *Service) DeleteEvent(ctx context.Context, requesterID string, eventID uuid.UUID, calendarID *uuid.UUID, scope DeletionScope) error {
	target, err := s.resolveMutationTarget(ctx, requesterID, eventID, calendarID)
	if err != nil {
		return err
	}
	if target.IsReadOnly {
		return ErrReadOnly
	}
	accessToken, err := s.fetchToken(ctx, target.TokenIdentity)
	if err != nil {
		return err
	}
	googleTarget := target.GoogleTarget(MaintenanceHorizon(time.Now().UTC()))
	var outcome *SeriesMutationOutcome
	switch scope.Kind {
	case "all", "":
		if err := s.provider.DeleteEvent(ctx, accessToken, googleTarget, target.MasterProviderEventID()); err != nil {
			return providerMutationError(err)
		}
		outcome = &SeriesMutationOutcome{SeriesDeleted: true}
	case "this_event":
		outcome, err = s.provider.DeleteEventInstance(ctx, accessToken, googleTarget, target.MasterProviderEventID(), scope.RecurrenceID)
		if err != nil {
			return providerMutationError(err)
		}
	case "this_and_following":
		outcome, err = s.provider.TruncateRecurringEvent(ctx, accessToken, googleTarget, target.MasterProviderEventID(), scope.RecurrenceID)
		if err != nil {
			return providerMutationError(err)
		}
	default:
		return mutationErr(ErrCodeInvalidInput, "unknown deletion scope "+scope.Kind)
	}
	switch {
	case outcome.Applied != nil:
		_, err := s.persistEcho(ctx, target.Actor, *outcome.Applied)
		return err
	default: // SeriesDeleted | Gone
		retired, err := s.repo.RemoveGoogleSource(ctx, target.AccountID, target.CalendarID, target.MasterProviderEventID())
		if err != nil {
			return &MutationError{ErrCodePersistFailed, fmt.Sprintf("calendar mutation was applied but local persistence failed: %v", err)}
		}
		s.announceRetirements(ctx, target.OwnerID, target.EmailLinkID, retired)
		return nil
	}
}

// RespondToEvent mirrors CalendarMutationServiceImpl::respond_to_event.
func (s *Service) RespondToEvent(ctx context.Context, requesterID string, eventID uuid.UUID, calendarID *uuid.UUID, response AttendeeResponseStatus, scope RsvpScope) (CalendarEvent, error) {
	target, err := s.resolveMutationTarget(ctx, requesterID, eventID, calendarID)
	if err != nil {
		return CalendarEvent{}, err
	}
	if target.IsReadOnly {
		return CalendarEvent{}, ErrReadOnly
	}
	if target.Actor == nil {
		return CalendarEvent{}, ErrNotAttendee
	}
	accessToken, err := s.fetchToken(ctx, target.TokenIdentity)
	if err != nil {
		return CalendarEvent{}, err
	}
	outcome, err := s.provider.RsvpEvent(ctx, accessToken,
		target.GoogleTarget(MaintenanceHorizon(time.Now().UTC())),
		target.MasterProviderEventID(), target.Actor, response, scope)
	if err != nil {
		return CalendarEvent{}, providerMutationError(err)
	}
	switch {
	case outcome.Applied != nil:
		if scope.ThisEvent != nil {
			return s.persistOccurrenceEcho(ctx, target.Actor, *outcome.Applied, *scope.ThisEvent)
		}
		return s.persistEcho(ctx, target.Actor, *outcome.Applied)
	case outcome.NotAttendee:
		return CalendarEvent{}, ErrNotAttendee
	default: // Gone
		s.retireGoneSource(ctx, target)
		return CalendarEvent{}, ErrMutationNotFound
	}
}

// ListVisibleCalendars mirrors CalendarMutationServiceImpl::list_visible_calendars.
func (s *Service) ListVisibleCalendars(ctx context.Context, requesterID string) ([]VisibleCalendar, error) {
	cals, err := s.repo.ListVisibleCalendars(ctx, requesterID)
	if err != nil {
		return nil, internalErr(err)
	}
	return cals, nil
}

// ---------------------------------------------------------------------------
// validation helpers (mutations.rs)
// ---------------------------------------------------------------------------

func validateTime(t *EventTime) error {
	if !t.IsValid() {
		return mutationErr(ErrCodeInvalidInput, "event end must be after its start")
	}
	return nil
}

func validateOutOfOfficeCreate(draft *CalendarEventDraft, target *CalendarCreationTarget) error {
	if !target.IsPrimary {
		return mutationErr(ErrCodeInvalidInput, "out-of-office events can only be created on your primary calendar")
	}
	if draft.Time.IsAllDay {
		return mutationErr(ErrCodeInvalidInput, "out-of-office events must have specific start and end times, not span whole days")
	}
	if len(draft.Attendees) > 0 {
		return mutationErr(ErrCodeInvalidInput, "out-of-office events cannot have attendees")
	}
	if draft.Conference != nil {
		return mutationErr(ErrCodeInvalidInput, "out-of-office events cannot have a video conference")
	}
	if draft.Description != nil && *draft.Description != "" {
		return mutationErr(ErrCodeInvalidInput, "out-of-office events cannot have a description")
	}
	return nil
}

func validateReminders(reminders *EventReminders) error {
	if reminders.UseDefault && len(reminders.Overrides) > 0 {
		return mutationErr(ErrCodeInvalidInput, "reminder overrides require useDefault to be off")
	}
	if len(reminders.Overrides) > ReminderOverridesMax {
		return mutationErr(ErrCodeInvalidInput, fmt.Sprintf("an event allows at most %d reminders", ReminderOverridesMax))
	}
	for _, r := range reminders.Overrides {
		if r.Method != ReminderMethodPopup && r.Method != ReminderMethodEmail {
			return mutationErr(ErrCodeInvalidInput, fmt.Sprintf("unsupported reminder method: %q", r.Method))
		}
		if r.Minutes > ReminderMinutesMax {
			return mutationErr(ErrCodeInvalidInput, fmt.Sprintf("a reminder can fire at most %d minutes before the event", ReminderMinutesMax))
		}
	}
	return nil
}

func ensureOrganizerAttendee(attendees *[]CalendarAttendeeInput, organizerEmail string) {
	kept := false
	out := (*attendees)[:0]
	for _, a := range *attendees {
		if !strings.EqualFold(a.Email, organizerEmail) {
			out = append(out, a)
			continue
		}
		if kept {
			continue
		}
		accepted := AttendeeAccepted
		a.ResponseStatus = &accepted
		kept = true
		out = append(out, a)
	}
	*attendees = out
	if !kept {
		accepted := AttendeeAccepted
		*attendees = append([]CalendarAttendeeInput{{
			Email:          organizerEmail,
			ResponseStatus: &accepted,
		}}, *attendees...)
	}
}

// preserveRetainedAttendeeState mirrors preserve_retained_attendee_state:
// stored entries are matched by email with later entries winning.
func preserveRetainedAttendeeState(attendees []CalendarAttendeeInput, stored []CalendarAttendee) {
	byEmail := map[string]*CalendarAttendee{}
	for i := range stored {
		byEmail[strings.ToLower(stored[i].Email)] = &stored[i]
	}
	for i := range attendees {
		existing, ok := byEmail[strings.ToLower(attendees[i].Email)]
		if !ok {
			continue
		}
		if attendees[i].ResponseStatus == nil {
			s := existing.ResponseStatus
			attendees[i].ResponseStatus = &s
		}
		if !attendees[i].IsOptional {
			attendees[i].IsOptional = existing.IsOptional
		}
	}
}

func validateAttendeeEmail(email string) error {
	trimmed := strings.TrimSpace(email)
	if trimmed == "" || !strings.Contains(trimmed, "@") {
		return mutationErr(ErrCodeInvalidInput, fmt.Sprintf("invalid attendee email: %q", email))
	}
	return nil
}

// ---------------------------------------------------------------------------
// NATS adapters
// ---------------------------------------------------------------------------

// NATSRefreshNotifier publishes `calendar_event.synced` refresh signals for a
// link's viewers onto realtime subjects (replaces CalendarRefreshNotifier's
// ConnectionGatewayClient path). Best-effort: delivery failures are logged.
type NATSRefreshNotifier struct {
	NC     *nats.Conn
	Lookup func(ctx context.Context, emailLinkID uuid.UUID) (owner string, viewers []string, err error)
}

// CalendarChanged implements RefreshNotifier.
func (n NATSRefreshNotifier) CalendarChanged(ctx context.Context, ownerID string, emailLinkID uuid.UUID) {
	payload, err := json.Marshal(map[string]any{
		"event":   "synced",
		"link_id": emailLinkID,
	})
	if err != nil {
		return
	}
	env, err := events.New("calendar.refresh", "calendar", emailLinkID.String(), 1, json.RawMessage(payload))
	if err != nil {
		return
	}
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	recipients := []string{ownerID}
	if n.Lookup != nil {
		owner, viewers, err := n.Lookup(ctx, emailLinkID)
		if err == nil {
			if owner != "" {
				recipients[0] = owner
			}
			recipients = append(recipients, viewers...)
		}
	}
	for _, uid := range recipients {
		if err := n.NC.Publish("realtime.user."+uid, data); err != nil {
			slog.Error("calendar: refresh publish failed", "uid", uid, "err", err)
		}
	}
}

// SubjectCalendarEvents is the JetStream stream subject for calendar entity
// events (the Go replacement for the Kafka "calendar" topic published through
// CalendarEventPubSub in Rust). Sharded per event id to preserve ordering.
const SubjectCalendarEvents = "calendar.events"

// NATSPublisher publishes calendar topic events as events.Envelope messages
// onto `calendar.events.<event_id>` via JetStream, replacing the Rust
// CalendarEventPubSub (Kafka) producer. A nil JetStream falls back to core
// NATS; a nil connection drops events silently.
type NATSPublisher struct {
	JS jetstream.JetStream
	NC *nats.Conn
}

// PublishCalendarEvent implements EventPublisher.
func (p NATSPublisher) PublishCalendarEvent(ctx context.Context, kind string, eventID uuid.UUID, ownerID string) {
	data, err := json.Marshal(CalendarEventMessage{Kind: kind, EventID: eventID, OwnerID: ownerID})
	if err != nil {
		return
	}
	env, err := events.New("calendar."+kind, "calendar", eventID.String(), 1, json.RawMessage(data))
	if err != nil {
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	subject := events.Shard(SubjectCalendarEvents, eventID.String())
	if p.JS != nil {
		if _, err := p.JS.Publish(ctx, subject, raw); err != nil {
			slog.Error("calendar: event publish failed", "subject", subject, "err", err)
		}
		return
	}
	if p.NC != nil {
		if err := p.NC.Publish(subject, raw); err != nil {
			slog.Error("calendar: event publish failed", "subject", subject, "err", err)
		}
	}
}
