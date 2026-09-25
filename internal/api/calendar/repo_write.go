package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ---------------------------------------------------------------------------
// write path — mirrors PgCalendarRepository::upsert_event (UserMutation) and
// remove_google_source. The fenced GoogleBackfill variant of upsert_event is
// TODO(backfill) and lands with the sync workers.
// ---------------------------------------------------------------------------

// eventReconciliationLock mirrors event_reconciliation_lock: stable FNV-1a
// over (source_link_id, ical_uid) so every service process serializes the
// same event on the same advisory-lock key.
func eventReconciliationLock(sourceLinkID uuid.UUID, icalUID string) int64 {
	hash := uint64(0xcbf29ce484222325)
	for _, b := range sourceLinkID {
		hash ^= uint64(b)
		hash *= 0x00000100000001b3
	}
	for i := 0; i < len(icalUID); i++ {
		hash ^= uint64(icalUID[i])
		hash *= 0x00000100000001b3
	}
	return int64(hash) // i64::from_ne_bytes equivalent
}

// canonicalProjection strips the volatile identifiers a fresh normalization
// mints so two projections of the same provider state compare equal
// (pg.rs::canonical_projection).
func canonicalProjection(raw []byte) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if event, ok := v["event"].(map[string]any); ok {
		event["id"] = nil
	}
	if occurrences, ok := v["occurrences"].([]any); ok {
		for _, o := range occurrences {
			if occ, ok := o.(map[string]any); ok {
				occ["eventId"] = nil
			}
		}
	}
	return json.Marshal(v)
}

func projectionsEqual(a, b []byte) (bool, error) {
	ca, err := canonicalProjection(a)
	if err != nil {
		return false, err
	}
	cb, err := canonicalProjection(b)
	if err != nil {
		return false, err
	}
	var va, vb any
	if err := json.Unmarshal(ca, &va); err != nil {
		return false, err
	}
	if err := json.Unmarshal(cb, &vb); err != nil {
		return false, err
	}
	ab, _ := json.Marshal(va)
	bb, _ := json.Marshal(vb)
	return string(ab) == string(bb), nil
}

// UpsertEvent applies a provider-echo upsert (CalendarEventWrite::UserMutation;
// the fenced GoogleBackfill variant is TODO(backfill)). Mirrors upsert_event:
// advisory lock → idempotency short-circuit → entity insert → source upsert
// with the sequence guard → canonical/schedule projections → reminder rebuild.
func (r *Repo) UpsertEvent(ctx context.Context, upsert CalendarEventUpsert, userMutation bool) (*CalendarEventWriteOutcome, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	source := upsert.Source
	sourceKind := "google"
	sourceLinkID := source.EmailLinkID

	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM pg_advisory_xact_lock($1)`,
		eventReconciliationLock(sourceLinkID, upsert.Event.ICalUID)); err != nil {
		return nil, err
	}

	// Idempotency short-circuit: identical incoming projection skips the write.
	var (
		existingEventID  pgtype.UUID
		existingPayload  []byte
		existingSequence int32
		hasExisting      bool
	)
	err = tx.QueryRow(ctx, `
		SELECT event_id, normalized_payload, source_sequence
		FROM calendar_event_sources
		WHERE source_kind = 'google'
		  AND account_id = $1
		  AND calendar_id = $2
		  AND provider_event_id = $3`,
		source.AccountID, source.CalendarID, source.ProviderEventID).
		Scan(&existingEventID, &existingPayload, &existingSequence)
	switch {
	case err == nil:
		hasExisting = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return nil, err
	}
	if hasExisting {
		incoming, err := json.Marshal(StoredSourceProjection{
			Event:       upsert.Event,
			Overrides:   upsert.Overrides,
			Occurrences: upsert.Occurrences,
		})
		if err != nil {
			return nil, err
		}
		equal, err := projectionsEqual(existingPayload, incoming)
		if err != nil {
			return nil, err
		}
		if equal {
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return &CalendarEventWriteOutcome{
				EventID: uuid.UUID(existingEventID.Bytes),
				OwnerID: upsert.Event.OwnerID,
				Change:  ChangeUnchanged,
			}, nil
		}
	}

	incomingSequence := int32(upsert.Event.Sequence)
	copyAdvanced := !hasExisting || incomingSequence > existingSequence

	st := splitEventTime(upsert.Event.Time)
	var confProvider pgtype.Text
	if upsert.Event.ConferenceProvider != nil {
		confProvider = pgtype.Text{String: string(*upsert.Event.ConferenceProvider), Valid: true}
	}
	reminderOverrides, err := json.Marshal(upsert.Event.Reminders.Overrides)
	if err != nil {
		return nil, err
	}

	// The entity row is created by whichever source arrives first; an existing
	// row is left alone — its content belongs to its canonical source.
	var insertedID pgtype.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO calendar_events (
			id, owner_id, source_link_id, ical_uid, title, description, location,
			status, visibility, transparency, event_type,
			starts_at, ends_at, start_date, end_date, time_zone,
			recurrence_lines, organizer_email, organizer_name,
			creator_email, creator_name,
			conference_url, conference_provider, sequence, is_read_only,
			canonical_source_kind,
			reminders_use_default, reminder_overrides,
			created_at, updated_at, schedule_updated_at
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $30,
			$11, $12, $13, $14, $15,
			$16, $17, $18,
			$28, $29,
			$19, $27, $20, $21, $22,
			$25, $26,
			$23, $24, $24
		)
		ON CONFLICT (owner_id, source_link_id, ical_uid) DO NOTHING
		RETURNING id`,
		upsert.Event.ID, upsert.Event.OwnerID, sourceLinkID, upsert.Event.ICalUID,
		upsert.Event.Title, textOf(upsert.Event.Description), textOf(upsert.Event.Location),
		string(upsert.Event.Status), string(upsert.Event.Visibility), string(upsert.Event.Transparency),
		st.startsAt, st.endsAt, st.startDate, st.endDate, st.timeZone,
		upsert.Event.RecurrenceLines, textOf(upsert.Event.OrganizerEmail), textOf(upsert.Event.OrganizerName),
		textOf(upsert.Event.ConferenceURL), incomingSequence, upsert.Event.IsReadOnly,
		sourceKind,
		upsert.Event.CreatedAt, upsert.Event.UpdatedAt,
		upsert.Event.Reminders.UseDefault, reminderOverrides,
		confProvider,
		textOf(upsert.Event.CreatorEmail), textOf(upsert.Event.CreatorName),
		string(upsert.Event.EventType),
	).Scan(&insertedID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	inserted := insertedID.Valid
	var eventID uuid.UUID
	if inserted {
		eventID = uuid.UUID(insertedID.Bytes)
	} else {
		if err := tx.QueryRow(ctx,
			`SELECT id FROM calendar_events WHERE owner_id = $1 AND source_link_id = $2 AND ical_uid = $3`,
			upsert.Event.OwnerID, sourceLinkID, upsert.Event.ICalUID).Scan(&eventID); err != nil {
			return nil, err
		}
	}

	// The source row carries this copy's own content under its own freshness
	// guard. A stale copy changes nothing at all.
	sourceID, err := persistSource(ctx, tx, eventID, &upsert)
	if err != nil {
		return nil, err
	}
	if sourceID == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &CalendarEventWriteOutcome{
			EventID: eventID, OwnerID: upsert.Event.OwnerID, Change: ChangeUnchanged,
		}, nil
	}

	canonical, err := canonicalSource(ctx, tx, eventID)
	if err != nil {
		return nil, err
	}
	schedule, err := scheduleOwner(ctx, tx, eventID)
	if err != nil {
		return nil, err
	}
	isCanonical := canonical != nil && canonical.UUID == *sourceID
	ownsSchedule := schedule.SourceID != nil && *schedule.SourceID == *sourceID
	var writesSchedule bool
	if isCanonical {
		writesSchedule = ownsSchedule || copyAdvanced || !upsert.Event.UpdatedAt.Before(schedule.UpdatedAt)
	} else if userMutation {
		writesSchedule = !upsert.Event.UpdatedAt.Before(schedule.UpdatedAt)
	} else {
		writesSchedule = ownsSchedule
	}
	if isCanonical {
		if err := applyContentProjection(ctx, tx, eventID, *sourceID, &upsert, sourceKind); err != nil {
			return nil, err
		}
	}
	if writesSchedule {
		if isCanonical {
			if err := applyScheduleProjection(ctx, tx, eventID, *sourceID, &upsert); err != nil {
				return nil, err
			}
		} else {
			if err := applyTimeProjection(ctx, tx, eventID, *sourceID, &upsert); err != nil {
				return nil, err
			}
		}
	}
	if isCanonical || writesSchedule {
		var canonicalCalID *uuid.UUID
		if canonical != nil {
			canonicalCalID = canonical.calendarID
		}
		if err := rebuildEntityReminderFirings(ctx, tx, eventID, canonicalCalID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	change := ChangeUpdated
	if inserted {
		change = ChangeCreated
	}
	return &CalendarEventWriteOutcome{
		EventID: eventID, OwnerID: upsert.Event.OwnerID, Change: change,
	}, nil
}

func textOf(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// persistSource mirrors persist_source.
func persistSource(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, upsert *CalendarEventUpsert) (*uuid.UUID, error) {
	normalized, err := json.Marshal(StoredSourceProjection{
		Event:       upsert.Event,
		Overrides:   upsert.Overrides,
		Occurrences: upsert.Occurrences,
	})
	if err != nil {
		return nil, err
	}
	source := upsert.Source
	event := upsert.Event
	rawPayload := source.RawPayload
	if len(rawPayload) == 0 {
		rawPayload = json.RawMessage(`{}`)
	}
	overrides, err := json.Marshal(event.Reminders.Overrides)
	if err != nil {
		return nil, err
	}
	var sourceID pgtype.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO calendar_event_sources (
			id, event_id, source_link_id, source_kind, account_id, calendar_id,
			provider_event_id, provider_recurring_event_id,
			provider_etag, raw_payload, source_sequence,
			source_updated_at, normalized_payload,
			title, description, location, event_type, visibility, transparency,
			is_read_only, reminders_use_default, reminder_overrides,
			creator_email, creator_name
		)
		VALUES (
			$1, $2, $3, 'google', $4, $5, $6, $7, $8, $9,
			$10, $11, $12,
			$13, $14, $15, $16, $17, $18,
			$19, $20, $21,
			$22, $23
		)
		ON CONFLICT (account_id, calendar_id, provider_event_id)
			WHERE source_kind = 'google'
		DO UPDATE SET
			event_id = EXCLUDED.event_id,
			provider_recurring_event_id = EXCLUDED.provider_recurring_event_id,
			provider_etag = EXCLUDED.provider_etag,
			raw_payload = EXCLUDED.raw_payload,
			source_sequence = EXCLUDED.source_sequence,
			source_updated_at = EXCLUDED.source_updated_at,
			normalized_payload = EXCLUDED.normalized_payload,
			title = EXCLUDED.title,
			description = EXCLUDED.description,
			location = EXCLUDED.location,
			event_type = EXCLUDED.event_type,
			visibility = EXCLUDED.visibility,
			transparency = EXCLUDED.transparency,
			is_read_only = EXCLUDED.is_read_only,
			reminders_use_default = EXCLUDED.reminders_use_default,
			reminder_overrides = EXCLUDED.reminder_overrides,
			creator_email = EXCLUDED.creator_email,
			creator_name = EXCLUDED.creator_name,
			last_seen_at = now()
		WHERE
			EXCLUDED.source_sequence > calendar_event_sources.source_sequence
			OR (
				EXCLUDED.source_sequence = calendar_event_sources.source_sequence
				AND EXCLUDED.source_updated_at >= calendar_event_sources.source_updated_at
			)
		RETURNING id`,
		uuid.Must(uuid.NewV7()), eventID, source.EmailLinkID, source.AccountID,
		source.CalendarID, source.ProviderEventID, textOf(source.ProviderRecurringEventID),
		textOf(source.ProviderETag), rawPayload, int32(event.Sequence),
		event.UpdatedAt, normalized,
		event.Title, textOf(event.Description), textOf(event.Location),
		string(event.EventType), string(event.Visibility), string(event.Transparency),
		event.IsReadOnly, event.Reminders.UseDefault, overrides,
		textOf(event.CreatorEmail), textOf(event.CreatorName),
	).Scan(&sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	id := uuid.UUID(sourceID.Bytes)
	return &id, nil
}

// canonicalSourceResult pairs the canonical source id with its calendar.
type canonicalSourceResult struct {
	uuid.UUID
	calendarID *uuid.UUID
}

// canonicalSource mirrors canonical_source (the DB function
// calendar_event_canonical_source_id owns the ranking).
func canonicalSource(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*canonicalSourceResult, error) {
	var id, calID pgtype.UUID
	err := tx.QueryRow(ctx, `
		SELECT source.id, source.calendar_id
		FROM calendar_event_sources source
		WHERE source.id = calendar_event_canonical_source_id($1)`,
		eventID).Scan(&id, &calID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	res := &canonicalSourceResult{UUID: uuid.UUID(id.Bytes)}
	if calID.Valid {
		c := uuid.UUID(calID.Bytes)
		res.calendarID = &c
	}
	return res, nil
}

// scheduleOwnerRow mirrors ScheduleOwner.
type scheduleOwnerRow struct {
	SourceID  *uuid.UUID
	UpdatedAt time.Time
}

func scheduleOwner(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (scheduleOwnerRow, error) {
	var sourceID pgtype.UUID
	var updatedAt time.Time
	err := tx.QueryRow(ctx,
		`SELECT schedule_source_id, schedule_updated_at FROM calendar_events WHERE id = $1`,
		eventID).Scan(&sourceID, &updatedAt)
	if err != nil {
		return scheduleOwnerRow{}, err
	}
	var id *uuid.UUID
	if sourceID.Valid {
		v := uuid.UUID(sourceID.Bytes)
		id = &v
	}
	return scheduleOwnerRow{SourceID: id, UpdatedAt: updatedAt}, nil
}

// applyContentProjection mirrors apply_content_projection.
func applyContentProjection(ctx context.Context, tx pgx.Tx, eventID, sourceID uuid.UUID, upsert *CalendarEventUpsert, sourceKind string) error {
	overrides, err := json.Marshal(upsert.Event.Reminders.Overrides)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE calendar_events
		SET content_source_id = $17,
			title = $2,
			description = $3,
			location = $4,
			visibility = $5,
			transparency = $6,
			event_type = $7,
			creator_email = $8,
			creator_name = $9,
			sequence = $10,
			is_read_only = $11,
			canonical_source_kind = $12,
			reminders_use_default = $13,
			reminder_overrides = $14,
			created_at = $15,
			updated_at = GREATEST(calendar_events.updated_at, $16)
		WHERE id = $1`,
		eventID,
		upsert.Event.Title, textOf(upsert.Event.Description), textOf(upsert.Event.Location),
		string(upsert.Event.Visibility), string(upsert.Event.Transparency), string(upsert.Event.EventType),
		textOf(upsert.Event.CreatorEmail), textOf(upsert.Event.CreatorName),
		int32(upsert.Event.Sequence), upsert.Event.IsReadOnly, sourceKind,
		upsert.Event.Reminders.UseDefault, overrides,
		upsert.Event.CreatedAt, upsert.Event.UpdatedAt,
		sourceID)
	return err
}

// applyScheduleProjection mirrors apply_schedule_projection.
func applyScheduleProjection(ctx context.Context, tx pgx.Tx, eventID, sourceID uuid.UUID, upsert *CalendarEventUpsert) error {
	st := splitEventTime(upsert.Event.Time)
	var confProvider pgtype.Text
	if upsert.Event.ConferenceProvider != nil {
		confProvider = pgtype.Text{String: string(*upsert.Event.ConferenceProvider), Valid: true}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE calendar_events
		SET status = $2,
			starts_at = $3,
			ends_at = $4,
			start_date = $5,
			end_date = $6,
			time_zone = $7,
			recurrence_lines = $8,
			organizer_email = $9,
			organizer_name = $10,
			conference_url = $11,
			conference_provider = $12,
			schedule_source_id = $13,
			schedule_updated_at = $14,
			updated_at = GREATEST(calendar_events.updated_at, $14)
		WHERE id = $1`,
		eventID,
		string(upsert.Event.Status),
		st.startsAt, st.endsAt, st.startDate, st.endDate, st.timeZone,
		upsert.Event.RecurrenceLines,
		textOf(upsert.Event.OrganizerEmail), textOf(upsert.Event.OrganizerName),
		textOf(upsert.Event.ConferenceURL), confProvider,
		sourceID, upsert.Event.UpdatedAt); err != nil {
		return err
	}
	if err := replaceAttendees(ctx, tx, eventID, upsert.Event.Attendees); err != nil {
		return err
	}
	if err := replaceOverrides(ctx, tx, eventID, upsert.Overrides); err != nil {
		return err
	}
	return replaceOccurrences(ctx, tx, eventID, upsert.Event.OwnerID, upsert.Occurrences)
}

// applyTimeProjection mirrors apply_time_projection.
func applyTimeProjection(ctx context.Context, tx pgx.Tx, eventID, sourceID uuid.UUID, upsert *CalendarEventUpsert) error {
	st := splitEventTime(upsert.Event.Time)
	if _, err := tx.Exec(ctx, `
		UPDATE calendar_events
		SET status = $2,
			starts_at = $3,
			ends_at = $4,
			start_date = $5,
			end_date = $6,
			time_zone = $7,
			recurrence_lines = $8,
			schedule_source_id = $9,
			schedule_updated_at = $10,
			updated_at = GREATEST(calendar_events.updated_at, $10)
		WHERE id = $1`,
		eventID,
		string(upsert.Event.Status),
		st.startsAt, st.endsAt, st.startDate, st.endDate, st.timeZone,
		upsert.Event.RecurrenceLines,
		sourceID, upsert.Event.UpdatedAt); err != nil {
		return err
	}
	if err := replaceOverrides(ctx, tx, eventID, upsert.Overrides); err != nil {
		return err
	}
	return replaceOccurrences(ctx, tx, eventID, upsert.Event.OwnerID, upsert.Occurrences)
}

// replaceAttendees mirrors replace_attendees.
func replaceAttendees(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, attendees []CalendarAttendee) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM calendar_event_attendees WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	for _, a := range attendees {
		if _, err := tx.Exec(ctx, `
			INSERT INTO calendar_event_attendees (
				event_id, email, display_name, response_status,
				is_organizer, is_optional, is_self, comment
			)
			VALUES ($1, lower($2), $3, $4, $5, $6, $7, $8)
			ON CONFLICT (event_id, email) DO UPDATE SET
				display_name = EXCLUDED.display_name,
				response_status = EXCLUDED.response_status,
				is_organizer = EXCLUDED.is_organizer,
				is_optional = EXCLUDED.is_optional,
				is_self = EXCLUDED.is_self,
				comment = EXCLUDED.comment`,
			eventID, a.Email, textOf(a.DisplayName), string(a.ResponseStatus),
			a.IsOrganizer, a.IsOptional, a.IsSelf, textOf(a.Comment)); err != nil {
			return err
		}
	}
	return nil
}

// replaceOverrides mirrors replace_overrides (including per-occurrence
// attendee lists in calendar_event_override_attendees).
func replaceOverrides(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, overrides []CalendarEventOverride) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM calendar_event_overrides WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	for _, o := range overrides {
		st := splitEventTime(o.Time)
		var origStarts pgtype.Timestamptz
		var origDate pgtype.Date
		if o.OriginalTime.IsAllDay {
			origDate = pgtype.Date{Time: o.OriginalTime.StartDate.Time, Valid: true}
		} else {
			origStarts = pgtype.Timestamptz{Time: o.OriginalTime.StartsAt, Valid: true}
		}
		var status pgtype.Text
		if o.Status != nil {
			status = pgtype.Text{String: string(*o.Status), Valid: true}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO calendar_event_overrides (
				event_id, recurrence_id, original_starts_at, original_start_date,
				starts_at, ends_at, start_date, end_date,
				title, description, location, status, attendees_overridden
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			eventID, o.RecurrenceID, origStarts, origDate,
			st.startsAt, st.endsAt, st.startDate, st.endDate,
			textOf(o.Title), textOf(o.Description), textOf(o.Location), status,
			o.Attendees != nil); err != nil {
			return err
		}
		if o.Attendees != nil {
			for _, a := range *o.Attendees {
				if _, err := tx.Exec(ctx, `
					INSERT INTO calendar_event_override_attendees (
						event_id, recurrence_id, email, display_name, response_status,
						is_organizer, is_optional, is_self, comment
					)
					VALUES ($1, $2, lower($3), $4, $5, $6, $7, $8, $9)
					ON CONFLICT (event_id, recurrence_id, email) DO UPDATE SET
						display_name = EXCLUDED.display_name,
						response_status = EXCLUDED.response_status,
						is_organizer = EXCLUDED.is_organizer,
						is_optional = EXCLUDED.is_optional,
						is_self = EXCLUDED.is_self,
						comment = EXCLUDED.comment`,
					eventID, o.RecurrenceID, a.Email, textOf(a.DisplayName),
					string(a.ResponseStatus), a.IsOrganizer, a.IsOptional, a.IsSelf,
					textOf(a.Comment)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// replaceOccurrences mirrors replace_occurrences.
func replaceOccurrences(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, ownerID string, occurrences []CalendarOccurrence) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM calendar_event_occurrences WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	for _, o := range occurrences {
		st := splitEventTime(o.Time)
		if _, err := tx.Exec(ctx, `
			INSERT INTO calendar_event_occurrences (
				event_id, owner_id, occurrence_key, recurrence_id,
				starts_at, ends_at, start_date, end_date, is_cancelled
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (event_id, occurrence_key) DO UPDATE SET
				owner_id = EXCLUDED.owner_id,
				recurrence_id = EXCLUDED.recurrence_id,
				starts_at = EXCLUDED.starts_at,
				ends_at = EXCLUDED.ends_at,
				start_date = EXCLUDED.start_date,
				end_date = EXCLUDED.end_date,
				is_cancelled = EXCLUDED.is_cancelled`,
			eventID, ownerID, o.OccurrenceKey, textOf(o.RecurrenceID),
			st.startsAt, st.endsAt, st.startDate, st.endDate, o.IsCancelled); err != nil {
			return err
		}
	}
	return nil
}

// calendarReminderContext mirrors CalendarReminderContext.
type calendarReminderContext struct {
	TimeZone         *string
	DefaultReminders []EventReminderOverride
}

func fetchCalendarReminderContext(ctx context.Context, tx pgx.Tx, calendarID *uuid.UUID) (*calendarReminderContext, error) {
	if calendarID == nil {
		return nil, nil
	}
	var tz pgtype.Text
	var defaultsJSON []byte
	err := tx.QueryRow(ctx,
		`SELECT time_zone, default_reminders FROM calendars WHERE id = $1`,
		*calendarID).Scan(&tz, &defaultsJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	ctxt := &calendarReminderContext{TimeZone: textPtr(tz)}
	if len(defaultsJSON) > 0 {
		if err := json.Unmarshal(defaultsJSON, &ctxt.DefaultReminders); err != nil {
			slog.Error("calendar: malformed default_reminders json", "calendar_id", calendarID, "err", err)
		}
	}
	return ctxt, nil
}

// anchorTimeZone mirrors anchor_time_zone: unknown zones fall back to UTC.
func anchorTimeZone(ctx context.Context, tx pgx.Tx, zone *string) (string, error) {
	if zone == nil || *zone == "UTC" {
		return "UTC", nil
	}
	var known bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)`,
		*zone).Scan(&known); err != nil {
		return "", err
	}
	if known {
		return *zone, nil
	}
	slog.Warn("calendar: time zone unknown to postgres, anchoring all-day reminders at UTC", "zone", *zone)
	return "UTC", nil
}

// rebuildEntityReminderFirings mirrors rebuild_entity_reminder_firings.
func rebuildEntityReminderFirings(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, canonicalCalendarID *uuid.UUID) error {
	var status, etype string
	var useDefault bool
	var overridesJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT status, event_type, reminders_use_default, reminder_overrides
		FROM calendar_events
		WHERE id = $1`, eventID).Scan(&status, &etype, &useDefault, &overridesJSON); err != nil {
		return err
	}
	reminders := EventReminders{UseDefault: useDefault}
	if len(overridesJSON) > 0 {
		if err := json.Unmarshal(overridesJSON, &reminders.Overrides); err != nil {
			slog.Error("calendar: malformed event reminder_overrides json", "event_id", eventID, "err", err)
		}
	}
	cal, err := fetchCalendarReminderContext(ctx, tx, canonicalCalendarID)
	if err != nil {
		return err
	}
	return rebuildEventReminderFirings(ctx, tx, eventID, eventStatusOf(status), eventTypeOf(etype), reminders, cal)
}

// rebuildEventReminderFirings mirrors rebuild_event_reminder_firings.
func rebuildEventReminderFirings(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, status EventStatus, etype EventType, reminders EventReminders, cal *calendarReminderContext) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM calendar_event_reminder_firings WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	if status == EventStatusCancelled {
		return nil
	}
	var defaults []EventReminderOverride
	if etype.UsesCalendarDefaultReminders() && cal != nil {
		defaults = cal.DefaultReminders
	}
	popup := reminders.PopupMinutes(defaults)
	if len(popup) == 0 {
		return nil
	}
	var zone *string
	if cal != nil {
		zone = cal.TimeZone
	}
	tz, err := anchorTimeZone(ctx, tx, zone)
	if err != nil {
		return err
	}
	minutes := make([]int32, len(popup))
	for i, m := range popup {
		minutes[i] = int32(m)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO calendar_event_reminder_firings (
			event_id, occurrence_key, minutes_before, fire_at
		)
		SELECT
			occurrence.event_id,
			occurrence.occurrence_key,
			offsets.minutes,
			COALESCE(
				occurrence.starts_at,
				occurrence.start_date::timestamp AT TIME ZONE $3
			) - make_interval(mins => offsets.minutes)
		FROM calendar_event_occurrences occurrence
		CROSS JOIN UNNEST($2::int[]) AS offsets(minutes)
		WHERE occurrence.event_id = $1
		  AND NOT occurrence.is_cancelled
		  AND COALESCE(
				occurrence.starts_at,
				occurrence.start_date::timestamp AT TIME ZONE $3
			  ) - make_interval(mins => offsets.minutes) > now() - interval '1 day'`,
		eventID, minutes, tz)
	return err
}

// RemoveGoogleSource mirrors remove_google_source: retire a provider source
// (a recurring master also retires its expanded instances), restoring the
// best surviving source or deleting the entity.
func (r *Repo) RemoveGoogleSource(ctx context.Context, accountID, calendarID uuid.UUID, providerEventID string) ([]RetiredCalendarEvent, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	cancelled := []string{providerEventID}
	rows, err := tx.Query(ctx, `
		WITH deleted_sources AS (
			DELETE FROM calendar_event_sources source
			WHERE source.source_kind = 'google'
			  AND source.account_id = $1
			  AND source.calendar_id = $2
			  AND (
					source.provider_event_id = ANY($3::text[])
					OR source.provider_recurring_event_id = ANY($3::text[])
			  )
			RETURNING source.event_id
		)
		SELECT DISTINCT event_id FROM deleted_sources`,
		accountID, calendarID, cancelled)
	if err != nil {
		return nil, err
	}
	var eventIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		eventIDs = append(eventIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var retired []RetiredCalendarEvent
	for _, id := range eventIDs {
		outcome, err := restoreBestSourceOrDelete(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			retired = append(retired, *outcome)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return retired, nil
}

// restoreBestSourceOrDelete mirrors restore_best_source_or_delete: rewrite the
// entity from its next-best remaining source, or delete it when none remains.
func restoreBestSourceOrDelete(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*RetiredCalendarEvent, error) {
	var sourceLinkID uuid.UUID
	var icalUID, ownerID string
	err := tx.QueryRow(ctx,
		`SELECT source_link_id, ical_uid, owner_id FROM calendar_events WHERE id = $1`,
		eventID).Scan(&sourceLinkID, &icalUID, &ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM pg_advisory_xact_lock($1)`,
		eventReconciliationLock(sourceLinkID, icalUID)); err != nil {
		return nil, err
	}

	var sourceID pgtype.UUID
	var sourceKind string
	var normalized []byte
	var calID pgtype.UUID
	err = tx.QueryRow(ctx, `
		SELECT source.id, source.source_kind, source.normalized_payload, source.calendar_id
		FROM calendar_event_sources source
		WHERE source.id = calendar_event_canonical_source_id($1)`,
		eventID).Scan(&sourceID, &sourceKind, &normalized, &calID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_events WHERE id = $1`, eventID); err != nil {
				return nil, err
			}
			return &RetiredCalendarEvent{EventID: eventID, OwnerID: ownerID, Deleted: true}, nil
		}
		return nil, err
	}
	var projection StoredSourceProjection
	if err := json.Unmarshal(normalized, &projection); err != nil {
		return nil, err
	}
	upsert := CalendarEventUpsert{
		Event:       projection.Event,
		Overrides:   projection.Overrides,
		Occurrences: projection.Occurrences,
	}
	sid := sourceID.Bytes
	if err := applyCanonicalProjection(ctx, tx, eventID, sid, &upsert, sourceKind, calID); err != nil {
		return nil, err
	}
	return &RetiredCalendarEvent{EventID: eventID, OwnerID: ownerID, Deleted: false}, nil
}

// applyCanonicalProjection mirrors apply_canonical_projection.
func applyCanonicalProjection(ctx context.Context, tx pgx.Tx, eventID, sourceID uuid.UUID, upsert *CalendarEventUpsert, sourceKind string, calID pgtype.UUID) error {
	if err := applyContentProjection(ctx, tx, eventID, sourceID, upsert, sourceKind); err != nil {
		return err
	}
	schedule, err := scheduleOwner(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if schedule.SourceID == nil || *schedule.SourceID == sourceID ||
		!upsert.Event.UpdatedAt.Before(schedule.UpdatedAt) {
		if err := applyScheduleProjection(ctx, tx, eventID, sourceID, upsert); err != nil {
			return err
		}
	}
	var cal *uuid.UUID
	if calID.Valid {
		c := uuid.UUID(calID.Bytes)
		cal = &c
	}
	return rebuildEntityReminderFirings(ctx, tx, eventID, cal)
}
