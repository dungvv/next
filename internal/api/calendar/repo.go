package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo is the CalendarRepository port backed by macrodb
// (outbound/pg.rs::PgCalendarRepository).
type Repo struct {
	pool *pgxpool.Pool
}

// NewRepo builds the macrodb-backed calendar repository.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// googleCalendarScopes mirrors GOOGLE_CALENDAR_SCOPES.
var googleCalendarScopes = []string{
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/calendar.calendarlist.readonly",
}

// ---------------------------------------------------------------------------
// row helpers
// ---------------------------------------------------------------------------

type splitTime struct {
	startsAt  pgtype.Timestamptz
	endsAt    pgtype.Timestamptz
	startDate pgtype.Date
	endDate   pgtype.Date
	timeZone  pgtype.Text
}

func splitEventTime(t EventTime) splitTime {
	if t.IsAllDay {
		return splitTime{
			startDate: pgtype.Date{Time: t.StartDate.Time, Valid: true},
			endDate:   pgtype.Date{Time: t.EndDate.Time, Valid: true},
		}
	}
	return splitTime{
		startsAt: pgtype.Timestamptz{Time: t.StartsAt, Valid: true},
		endsAt:   pgtype.Timestamptz{Time: t.EndsAt, Valid: true},
		timeZone: pgtype.Text{String: derefStr(t.TimeZone), Valid: t.TimeZone != nil},
	}
}

func rowTime(startsAt, endsAt pgtype.Timestamptz, startDate, endDate pgtype.Date, tz pgtype.Text) (EventTime, error) {
	switch {
	case startsAt.Valid && endsAt.Valid && !startDate.Valid && !endDate.Valid:
		t := EventTime{StartsAt: startsAt.Time, EndsAt: endsAt.Time}
		if tz.Valid {
			s := tz.String
			t.TimeZone = &s
		}
		return t, nil
	case !startsAt.Valid && !endsAt.Valid && startDate.Valid && endDate.Valid:
		return EventTime{IsAllDay: true, StartDate: Date{startDate.Time}, EndDate: Date{endDate.Time}}, nil
	default:
		return EventTime{}, fmt.Errorf("invalid calendar time shape in database")
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func uuidStr(id uuid.UUID) string { return id.String() }

func uuidStrs(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func eventStatusOf(s string) EventStatus {
	switch s {
	case "tentative":
		return EventStatusTentative
	case "cancelled":
		return EventStatusCancelled
	default:
		return EventStatusConfirmed
	}
}

func eventVisibilityOf(s string) EventVisibility {
	switch s {
	case "public":
		return EventVisibilityPublic
	case "private":
		return EventVisibilityPrivate
	case "confidential":
		return EventVisibilityConfidential
	default:
		return EventVisibilityDefault
	}
}

func eventTransparencyOf(s string) EventTransparency {
	if s == "transparent" {
		return EventTransparencyTransparent
	}
	return EventTransparencyOpaque
}

func eventTypeOf(s string) EventType {
	switch s {
	case "out_of_office":
		return EventTypeOutOfOffice
	case "focus_time":
		return EventTypeFocusTime
	case "working_location":
		return EventTypeWorkingLocation
	case "birthday":
		return EventTypeBirthday
	case "from_gmail":
		return EventTypeFromGmail
	default:
		return EventTypeDefault
	}
}

func attendeeStatusOf(s string) AttendeeResponseStatus {
	switch s {
	case "accepted":
		return AttendeeAccepted
	case "declined":
		return AttendeeDeclined
	case "tentative":
		return AttendeeTentative
	default:
		return AttendeeNeedsAction
	}
}

func conferenceProviderOf(s string) ConferenceProvider {
	if s == "google_meet" {
		return ConferenceProviderGoogleMeet
	}
	return ConferenceProviderOther
}

// ---------------------------------------------------------------------------
// read queries
// ---------------------------------------------------------------------------

// occurrenceJoinRow mirrors OccurrenceJoinRow.
type occurrenceJoinRow struct {
	eventID             uuid.UUID
	occurrenceKey       string
	recurrenceID        pgtype.Text
	occStartsAt         pgtype.Timestamptz
	occEndsAt           pgtype.Timestamptz
	occStartDate        pgtype.Date
	occEndDate          pgtype.Date
	isCancelled         bool
	overrideTitle       pgtype.Text
	overrideDescription pgtype.Text
	overrideLocation    pgtype.Text
	overrideStatus      pgtype.Text
	ownerID             string
	icalUID             string
	title               string
	description         pgtype.Text
	location            pgtype.Text
	status              string
	visibility          string
	transparency        string
	eventType           string
	startsAt            pgtype.Timestamptz
	endsAt              pgtype.Timestamptz
	startDate           pgtype.Date
	endDate             pgtype.Date
	timeZone            pgtype.Text
	recurrenceLines     []string
	organizerEmail      pgtype.Text
	organizerName       pgtype.Text
	creatorEmail        pgtype.Text
	creatorName         pgtype.Text
	conferenceURL       pgtype.Text
	conferenceProvider  pgtype.Text
	sequence            int32
	isReadOnly          bool
	remindersUseDefault bool
	reminderOverrides   []byte
	createdAt           time.Time
	updatedAt           time.Time
}

const occurrenceSelect = `
	SELECT
		occurrence.event_id,
		occurrence.occurrence_key,
		occurrence.recurrence_id,
		occurrence.starts_at AS occurrence_starts_at,
		occurrence.ends_at AS occurrence_ends_at,
		occurrence.start_date AS occurrence_start_date,
		occurrence.end_date AS occurrence_end_date,
		occurrence.is_cancelled,
		override.title AS override_title,
		override.description AS override_description,
		override.location AS override_location,
		override.status AS override_status,
		event.owner_id,
		event.ical_uid,
		event.title,
		event.description,
		event.location,
		event.status,
		event.visibility,
		event.transparency,
		event.event_type,
		event.starts_at,
		event.ends_at,
		event.start_date,
		event.end_date,
		event.time_zone,
		event.recurrence_lines,
		event.organizer_email,
		event.organizer_name,
		event.creator_email,
		event.creator_name,
		event.conference_url,
		event.conference_provider,
		event.sequence,
		event.is_read_only,
		event.reminders_use_default,
		event.reminder_overrides,
		event.created_at,
		event.updated_at
	FROM calendar_event_occurrences occurrence
	JOIN calendar_events event ON event.id = occurrence.event_id
	LEFT JOIN calendar_event_overrides override
		ON override.event_id = occurrence.event_id
	   AND override.recurrence_id = occurrence.recurrence_id
`

func scanOccurrenceJoin(row pgx.Row) (occurrenceJoinRow, error) {
	var r occurrenceJoinRow
	err := row.Scan(
		&r.eventID, &r.occurrenceKey, &r.recurrenceID,
		&r.occStartsAt, &r.occEndsAt, &r.occStartDate, &r.occEndDate,
		&r.isCancelled,
		&r.overrideTitle, &r.overrideDescription, &r.overrideLocation, &r.overrideStatus,
		&r.ownerID, &r.icalUID, &r.title, &r.description, &r.location,
		&r.status, &r.visibility, &r.transparency, &r.eventType,
		&r.startsAt, &r.endsAt, &r.startDate, &r.endDate, &r.timeZone,
		&r.recurrenceLines, &r.organizerEmail, &r.organizerName,
		&r.creatorEmail, &r.creatorName,
		&r.conferenceURL, &r.conferenceProvider, &r.sequence, &r.isReadOnly,
		&r.remindersUseDefault, &r.reminderOverrides,
		&r.createdAt, &r.updatedAt,
	)
	return r, err
}

func (r occurrenceJoinRow) occurrence() (CalendarOccurrence, error) {
	t, err := rowTime(r.occStartsAt, r.occEndsAt, r.occStartDate, r.occEndDate, pgtype.Text{})
	if err != nil {
		return CalendarOccurrence{}, err
	}
	return CalendarOccurrence{
		EventID:       r.eventID,
		OccurrenceKey: r.occurrenceKey,
		RecurrenceID:  textPtr(r.recurrenceID),
		Time:          t,
		IsCancelled:   r.isCancelled,
	}, nil
}

func (r occurrenceJoinRow) event(attendees []CalendarAttendee, sources []CalendarEventSourceContent) (CalendarEvent, error) {
	t, err := rowTime(r.startsAt, r.endsAt, r.startDate, r.endDate, r.timeZone)
	if err != nil {
		return CalendarEvent{}, err
	}
	var overrides []EventReminderOverride
	if len(r.reminderOverrides) > 0 {
		if err := json.Unmarshal(r.reminderOverrides, &overrides); err != nil {
			slog.Error("calendar: malformed event reminder_overrides json", "event_id", r.eventID, "err", err)
		}
	}
	var calID *uuid.UUID
	if len(sources) > 0 {
		id := sources[0].CalendarID
		calID = &id
	}
	e := CalendarEvent{
		ID:              r.eventID,
		OwnerID:         r.ownerID,
		ICalUID:         r.icalUID,
		CalendarID:      calID,
		Sources:         sources,
		Title:           r.title,
		Description:     textPtr(r.description),
		Location:        textPtr(r.location),
		Status:          eventStatusOf(r.status),
		Visibility:      eventVisibilityOf(r.visibility),
		Transparency:    eventTransparencyOf(r.transparency),
		EventType:       eventTypeOf(r.eventType),
		Time:            t,
		RecurrenceLines: r.recurrenceLines,
		OrganizerEmail:  textPtr(r.organizerEmail),
		OrganizerName:   textPtr(r.organizerName),
		CreatorEmail:    textPtr(r.creatorEmail),
		CreatorName:     textPtr(r.creatorName),
		ConferenceURL:   textPtr(r.conferenceURL),
		Sequence:        uint32(max(r.sequence, 0)),
		IsReadOnly:      r.isReadOnly,
		Reminders:       EventReminders{UseDefault: r.remindersUseDefault, Overrides: overrides},
		Attendees:       attendees,
		CreatedAt:       r.createdAt,
		UpdatedAt:       r.updatedAt,
	}
	if r.conferenceProvider.Valid {
		p := conferenceProviderOf(r.conferenceProvider.String)
		e.ConferenceProvider = &p
	}
	// An exception's content replaces the series content for that occurrence.
	var ostatus *EventStatus
	if r.overrideStatus.Valid {
		s := eventStatusOf(r.overrideStatus.String)
		ostatus = &s
	}
	e.ApplyOccurrenceContent(textPtr(r.overrideTitle), textPtr(r.overrideDescription), textPtr(r.overrideLocation), ostatus)
	return e, nil
}

// ListOccurrences mirrors PgCalendarRepository::list_occurrences: occurrences
// visible to the requester across owned and delegated inboxes, keyset-paginated.
func (r *Repo) ListOccurrences(ctx context.Context, requesterID string, rng OccurrenceRange, cursor *CalendarOccurrenceCursor, limit int) ([]struct {
	Event      CalendarEvent
	Occurrence CalendarOccurrence
}, error) {
	var cursorStarts pgtype.Timestamptz
	var cursorEventID pgtype.UUID
	var cursorKey pgtype.Text
	if cursor != nil {
		cursorStarts = pgtype.Timestamptz{Time: cursor.StartsAt, Valid: true}
		cursorEventID = pgtype.UUID{Bytes: cursor.EventID, Valid: true}
		cursorKey = pgtype.Text{String: cursor.OccurrenceKey, Valid: true}
	}
	rows, err := r.pool.Query(ctx, occurrenceSelect+`
		WHERE occurrence.owner_id IN (
				SELECT $1::text
				UNION
				SELECT link.child_macro_id
				FROM macro_user_links link
				WHERE link.primary_macro_id = $1
		  )
		  AND event.status <> 'cancelled'
		  AND NOT occurrence.is_cancelled
		  AND (
				event.owner_id = $1
				OR EXISTS (
					SELECT 1
					FROM macro_user_links link
					WHERE link.link_id = event.source_link_id
					  AND link.primary_macro_id = $1
				)
		  )
		  AND (
				occurrence.timed_span && tstzrange($2, $3, '[)')
				OR occurrence.day_span && daterange($4, $5, '[)')
		  )
		  AND (
				$6::timestamptz IS NULL
				OR (
					COALESCE(
						occurrence.starts_at,
						occurrence.start_date::timestamp AT TIME ZONE 'UTC'
					),
					occurrence.event_id,
					occurrence.occurrence_key
				) > ($6, $7, $8)
		  )
		ORDER BY
			COALESCE(occurrence.starts_at, occurrence.start_date::timestamp AT TIME ZONE 'UTC'),
			occurrence.event_id,
			occurrence.occurrence_key
		LIMIT $9`,
		requesterID,
		rng.StartsAt, rng.EndsAt,
		pgtype.Date{Time: rng.StartDate.Time, Valid: true},
		pgtype.Date{Time: rng.EndDate.Time, Valid: true},
		cursorStarts, cursorEventID, cursorKey,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var join []occurrenceJoinRow
	for rows.Next() {
		row, err := scanOccurrenceJoin(rows)
		if err != nil {
			return nil, err
		}
		join = append(join, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	eventIDs := make([]uuid.UUID, len(join))
	for i, row := range join {
		eventIDs[i] = row.eventID
	}
	attendees, err := fetchAttendees(ctx, r.pool, eventIDs)
	if err != nil {
		return nil, err
	}
	overrideAttendees, err := fetchOverrideAttendees(ctx, r.pool, eventIDs)
	if err != nil {
		return nil, err
	}
	sources, err := fetchSourceContents(ctx, r.pool, eventIDs)
	if err != nil {
		return nil, err
	}

	out := make([]struct {
		Event      CalendarEvent
		Occurrence CalendarOccurrence
	}, 0, len(join))
	for _, row := range join {
		occ, err := row.occurrence()
		if err != nil {
			return nil, err
		}
		// An exception's attendee list replaces the series list for that
		// occurrence alone.
		effective := attendees[row.eventID]
		if row.recurrenceID.Valid {
			if oa, ok := overrideAttendees[overrideKey{row.eventID, row.recurrenceID.String}]; ok {
				effective = oa
			}
		}
		ev, err := row.event(effective, sources[row.eventID])
		if err != nil {
			return nil, err
		}
		out = append(out, struct {
			Event      CalendarEvent
			Occurrence CalendarOccurrence
		}{ev, occ})
	}
	return out, nil
}

// ListTeamOutOfOffice mirrors list_team_out_of_office.
func (r *Repo) ListTeamOutOfOffice(ctx context.Context, requesterID string, rng OccurrenceRange, limit int) ([]TeamOutOfOffice, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			owner_id, event_id, ical_uid, occurrence_key, title, visibility,
			time_zone,
			occurrence_starts_at, occurrence_ends_at,
			occurrence_start_date, occurrence_end_date
		FROM (
			SELECT DISTINCT ON (event.owner_id, event.ical_uid, occurrence.occurrence_key)
				event.owner_id,
				occurrence.event_id,
				event.ical_uid,
				occurrence.occurrence_key,
				source.title,
				source.visibility,
				event.time_zone,
				occurrence.starts_at AS occurrence_starts_at,
				occurrence.ends_at AS occurrence_ends_at,
				occurrence.start_date AS occurrence_start_date,
				occurrence.end_date AS occurrence_end_date,
				COALESCE(
					occurrence.starts_at,
					occurrence.start_date::timestamp AT TIME ZONE 'UTC'
				) AS occurrence_ordering
			FROM calendar_event_occurrences occurrence
			JOIN calendar_events event ON event.id = occurrence.event_id
			JOIN calendar_event_sources source ON source.event_id = event.id
			JOIN calendars calendar
			  ON calendar.id = source.calendar_id
			 AND calendar.is_primary
			 AND NOT calendar.is_deleted
			JOIN calendar_accounts account
			  ON account.id = source.account_id
			 AND account.sync_status <> 'disabled'
			WHERE occurrence.owner_id IN (
					SELECT teammate.user_id
					FROM team_user membership
					JOIN team_user teammate ON teammate.team_id = membership.team_id
					WHERE membership.user_id = $1
					  AND teammate.user_id <> $1
			  )
			  AND source.event_type = 'out_of_office'
			  AND event.status <> 'cancelled'
			  AND NOT occurrence.is_cancelled
			  AND (
					occurrence.timed_span && tstzrange($2, $3, '[)')
					OR occurrence.day_span && daterange($4, $5, '[)')
			  )
			ORDER BY
				event.owner_id,
				event.ical_uid,
				occurrence.occurrence_key,
				occurrence.event_id,
				source.source_sequence DESC,
				source.source_updated_at DESC,
				source.id DESC
		) occurrence
		ORDER BY
			occurrence.occurrence_ordering,
			occurrence.event_id,
			occurrence.occurrence_key
		LIMIT $6`,
		requesterID, rng.StartsAt, rng.EndsAt,
		pgtype.Date{Time: rng.StartDate.Time, Valid: true},
		pgtype.Date{Time: rng.EndDate.Time, Valid: true},
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamOutOfOffice
	for rows.Next() {
		var (
			o         TeamOutOfOffice
			tz        pgtype.Text
			startsAt  pgtype.Timestamptz
			endsAt    pgtype.Timestamptz
			startDate pgtype.Date
			endDate   pgtype.Date
			title     string
			vis       string
		)
		if err := rows.Scan(&o.OwnerID, &o.EventID, &o.ICalUID, &o.OccurrenceKey,
			&title, &vis, &tz, &startsAt, &endsAt, &startDate, &endDate); err != nil {
			return nil, err
		}
		t, err := rowTime(startsAt, endsAt, startDate, endDate, tz)
		if err != nil {
			return nil, err
		}
		o.Title = &title
		o.Visibility = eventVisibilityOf(vis)
		o.Time = t
		out = append(out, o)
	}
	return out, rows.Err()
}

// mentionPreviewRow mirrors MentionPreviewRow.
type mentionPreviewRow struct {
	mentionExists   bool
	viewerEventID   pgtype.UUID
	title           pgtype.Text
	location        pgtype.Text
	organizerEmail  pgtype.Text
	organizerName   pgtype.Text
	recurrenceLines []string
	eventStartsAt   pgtype.Timestamptz
	eventEndsAt     pgtype.Timestamptz
	eventStartDate  pgtype.Date
	eventEndDate    pgtype.Date
	timeZone        pgtype.Text
	updatedAt       pgtype.Timestamptz
	occurrenceKey   pgtype.Text
	occStartsAt     pgtype.Timestamptz
	occEndsAt       pgtype.Timestamptz
	occStartDate    pgtype.Date
	occEndDate      pgtype.Date
	attendeeCount   pgtype.Int8
}

// MentionPreviews mirrors mention_previews: resolve mentioned events to the
// requester's own projections, one result per item in order.
func (r *Repo) MentionPreviews(ctx context.Context, requesterID string, items []CalendarMentionRequestItem, now time.Time) ([]CalendarMentionPreview, error) {
	eventIDs := make([]string, len(items))
	occKeys := make([]*string, len(items))
	for i, it := range items {
		eventIDs[i] = it.EventID.String()
		occKeys[i] = it.OccurrenceKey
	}
	rows, err := r.pool.Query(ctx, `
		SELECT
			(mentioned.id IS NOT NULL) AS mention_exists,
			viewer_event.id AS viewer_event_id,
			viewer_event.title AS title,
			viewer_event.location AS location,
			viewer_event.organizer_email AS organizer_email,
			viewer_event.organizer_name AS organizer_name,
			viewer_event.recurrence_lines AS recurrence_lines,
			viewer_event.starts_at AS event_starts_at,
			viewer_event.ends_at AS event_ends_at,
			viewer_event.start_date AS event_start_date,
			viewer_event.end_date AS event_end_date,
			viewer_event.time_zone AS time_zone,
			viewer_event.updated_at AS updated_at,
			occurrence.occurrence_key AS occurrence_key,
			occurrence.starts_at AS occurrence_starts_at,
			occurrence.ends_at AS occurrence_ends_at,
			occurrence.start_date AS occurrence_start_date,
			occurrence.end_date AS occurrence_end_date,
			attendees.attendee_count AS attendee_count
		FROM unnest($2::uuid[], $3::text[])
			WITH ORDINALITY AS requested(event_id, occurrence_key, ord)
		LEFT JOIN calendar_events mentioned
			ON mentioned.id = requested.event_id
		   AND mentioned.status <> 'cancelled'
		LEFT JOIN LATERAL (
			SELECT
				candidate.id,
				candidate.title,
				candidate.location,
				candidate.organizer_email,
				candidate.organizer_name,
				candidate.recurrence_lines,
				candidate.starts_at,
				candidate.ends_at,
				candidate.start_date,
				candidate.end_date,
				candidate.time_zone,
				candidate.updated_at
			FROM calendar_events candidate
			WHERE candidate.ical_uid = mentioned.ical_uid
			  AND candidate.status <> 'cancelled'
			  AND (
					candidate.owner_id = $1
					OR EXISTS (
						SELECT 1
						FROM macro_user_links link
						WHERE link.link_id = candidate.source_link_id
						  AND link.primary_macro_id = $1
					)
			  )
			ORDER BY
				(candidate.owner_id = $1) DESC,
				(candidate.id = mentioned.id) DESC,
				candidate.updated_at DESC,
				candidate.id
			LIMIT 1
		) viewer_event ON true
		LEFT JOIN LATERAL (
			SELECT
				instance.occurrence_key,
				instance.starts_at,
				instance.ends_at,
				instance.start_date,
				instance.end_date
			FROM calendar_event_occurrences instance
			CROSS JOIN LATERAL (
				SELECT COALESCE(
					instance.starts_at,
					instance.start_date::timestamp AT TIME ZONE 'UTC'
				) AS at
			) instance_start
			WHERE instance.event_id = viewer_event.id
			  AND NOT instance.is_cancelled
			ORDER BY
				(instance.occurrence_key
					IS NOT DISTINCT FROM requested.occurrence_key) DESC,
				(instance_start.at >= $4) DESC,
				CASE WHEN instance_start.at >= $4 THEN instance_start.at END ASC,
				instance_start.at DESC,
				instance.occurrence_key
			LIMIT 1
		) occurrence ON true
		LEFT JOIN LATERAL (
			SELECT count(*) AS attendee_count
			FROM calendar_event_attendees attendee
			WHERE attendee.event_id = viewer_event.id
		) attendees ON true
		ORDER BY requested.ord`,
		requesterID, eventIDs, occKeys, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalendarMentionPreview
	for rows.Next() {
		var row mentionPreviewRow
		if err := rows.Scan(
			&row.mentionExists, &row.viewerEventID, &row.title, &row.location,
			&row.organizerEmail, &row.organizerName, &row.recurrenceLines,
			&row.eventStartsAt, &row.eventEndsAt, &row.eventStartDate, &row.eventEndDate,
			&row.timeZone, &row.updatedAt,
			&row.occurrenceKey, &row.occStartsAt, &row.occEndsAt,
			&row.occStartDate, &row.occEndDate, &row.attendeeCount,
		); err != nil {
			return nil, err
		}
		preview, err := mentionPreviewFromRow(&row)
		if err != nil {
			return nil, err
		}
		out = append(out, preview)
	}
	return out, rows.Err()
}

func mentionPreviewFromRow(row *mentionPreviewRow) (CalendarMentionPreview, error) {
	if !row.mentionExists {
		return CalendarMentionPreview{Kind: MentionDoesNotExist}, nil
	}
	if !row.viewerEventID.Valid {
		return CalendarMentionPreview{Kind: MentionNoAccess}, nil
	}
	var t EventTime
	var err error
	if row.occurrenceKey.Valid {
		t, err = rowTime(row.occStartsAt, row.occEndsAt, row.occStartDate, row.occEndDate, row.timeZone)
	} else {
		// No materialized instance — the series' own span still previews.
		t, err = rowTime(row.eventStartsAt, row.eventEndsAt, row.eventStartDate, row.eventEndDate, row.timeZone)
	}
	if err != nil {
		return CalendarMentionPreview{}, err
	}
	return CalendarMentionPreview{
		Kind: MentionAccessible,
		Event: &CalendarMentionEvent{
			ViewerEventID:  row.viewerEventID.Bytes,
			Title:          row.title.String,
			Time:           t,
			OccurrenceKey:  textPtr(row.occurrenceKey),
			IsRecurring:    len(row.recurrenceLines) > 0,
			Location:       textPtr(row.location),
			OrganizerEmail: textPtr(row.organizerEmail),
			OrganizerName:  textPtr(row.organizerName),
			AttendeeCount:  int(row.attendeeCount.Int64),
			UpdatedAt:      row.updatedAt.Time,
		},
	}, nil
}

// SyncStatus mirrors sync_status.
func (r *Repo) SyncStatus(ctx context.Context, requesterID string) (CalendarSyncStatus, error) {
	var syncing bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM calendar_accounts account
			WHERE account.sync_status IN ('pending', 'syncing')
			  AND (
					account.owner_id = $1
					OR EXISTS (
						SELECT 1
						FROM macro_user_links link
						WHERE link.link_id = account.email_link_id
						  AND link.primary_macro_id = $1
					)
			  )
		)`, requesterID).Scan(&syncing)
	if err != nil {
		return "", err
	}
	if syncing {
		return SyncStatusSyncing, nil
	}
	return SyncStatusReady, nil
}

// ListVisibleCalendars mirrors list_visible_calendars.
func (r *Repo) ListVisibleCalendars(ctx context.Context, requesterID string) ([]VisibleCalendar, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			calendar.id,
			link.id AS email_link_id,
			link.email_address,
			calendar.name,
			calendar.color,
			calendar.is_primary,
			calendar.access_role,
			calendar.provider_calendar_id,
			calendar.default_reminders,
			calendar.last_sync_error,
			calendar.consecutive_sync_failures
		FROM email_links link
		JOIN calendar_accounts account ON account.email_link_id = link.id
		JOIN calendars calendar ON calendar.account_id = account.id
		WHERE NOT calendar.is_deleted
		  AND account.sync_status <> 'disabled'
		  AND (
				link.macro_id = $1
				OR EXISTS (
					SELECT 1
					FROM macro_user_links delegation
					WHERE delegation.link_id = link.id
					  AND delegation.primary_macro_id = $1
				)
		  )
		ORDER BY
			(link.macro_id = $1) DESC,
			link.is_primary DESC,
			link.created_at ASC,
			calendar.is_primary DESC,
			(calendar.access_role IN ('owner', 'writer')) DESC,
			calendar.name ASC`,
		requesterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VisibleCalendar
	for rows.Next() {
		var (
			c             VisibleCalendar
			color         pgtype.Text
			accessRole    pgtype.Text
			providerID    string
			defaultsJSON  []byte
			lastSyncError pgtype.Text
			failures      int32
		)
		if err := rows.Scan(&c.ID, &c.EmailLinkID, &c.EmailAddress, &c.Name,
			&color, &c.IsPrimary, &accessRole, &providerID,
			&defaultsJSON, &lastSyncError, &failures); err != nil {
			return nil, err
		}
		c.Color = textPtr(color)
		c.IsWritable = accessRole.Valid && (accessRole.String == "owner" || accessRole.String == "writer")
		c.IsSubscription = IsSystemCalendar(providerID)
		if lastSyncError.Valid && failures >= CalendarSyncFailureBadgeThreshold {
			c.SyncError = textPtr(lastSyncError)
		}
		if len(defaultsJSON) > 0 {
			if err := json.Unmarshal(defaultsJSON, &c.DefaultReminders); err != nil {
				slog.Error("calendar: malformed default_reminders json", "calendar_id", c.ID, "err", err)
			}
		}
		if c.DefaultReminders == nil {
			c.DefaultReminders = []EventReminderOverride{}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PrimaryTimeZone mirrors primary_time_zone.
func (r *Repo) PrimaryTimeZone(ctx context.Context, requesterID string) (*string, error) {
	var tz pgtype.Text
	err := r.pool.QueryRow(ctx, `
		SELECT calendar.time_zone
		FROM email_links link
		JOIN calendar_accounts account ON account.email_link_id = link.id
		JOIN calendars calendar ON calendar.account_id = account.id
		WHERE NOT calendar.is_deleted
		  AND account.sync_status <> 'disabled'
		  AND calendar.is_primary
		  AND (
				link.macro_id = $1
				OR EXISTS (
					SELECT 1
					FROM macro_user_links delegation
					WHERE delegation.link_id = link.id
					  AND delegation.primary_macro_id = $1
				)
		  )
		ORDER BY
			(link.macro_id = $1) DESC,
			link.is_primary DESC,
			link.created_at ASC
		LIMIT 1`, requesterID).Scan(&tz)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return textPtr(tz), nil
}

// GetEventMutationTarget mirrors get_event_mutation_target.
func (r *Repo) GetEventMutationTarget(ctx context.Context, requesterID string, eventID uuid.UUID, calendarID *uuid.UUID) (*CalendarEventMutationTarget, error) {
	var calParam pgtype.UUID
	if calendarID != nil {
		calParam = pgtype.UUID{Bytes: *calendarID, Valid: true}
	}
	var (
		t            CalendarEventMutationTarget
		recurringID  pgtype.Text
		faUserID     pgtype.Text
		emailAddress string
		provider     string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT
			event.id AS event_id,
			event.owner_id,
			source.is_read_only,
			source.provider_event_id,
			source.provider_recurring_event_id,
			source.account_id,
			source.calendar_id,
			calendar.provider_calendar_id,
			account.email_link_id,
			link.fusionauth_user_id,
			link.email_address,
			link.provider::text AS provider
		FROM calendar_events event
		JOIN calendar_event_sources source
			ON source.event_id = event.id
		   AND source.source_kind = 'google'
		JOIN calendars calendar ON calendar.id = source.calendar_id
		JOIN calendar_accounts account ON account.id = source.account_id
		JOIN email_links link ON link.id = account.email_link_id
		WHERE event.id = $1
		  AND NOT calendar.is_deleted
		  AND account.sync_status <> 'disabled'
		  AND CASE
				WHEN $3::uuid IS NULL
					THEN source.id = calendar_event_canonical_source_id(event.id)
				ELSE source.calendar_id = $3
			  END
		  AND (
				event.owner_id = $2
				OR EXISTS (
					SELECT 1
					FROM macro_user_links delegation
					WHERE delegation.link_id = event.source_link_id
					  AND delegation.primary_macro_id = $2
				)
		  )
		ORDER BY
			source.source_sequence DESC,
			source.source_updated_at DESC,
			source.id DESC
		LIMIT 1`,
		eventID, requesterID, calParam).Scan(
		&t.EventID, &t.OwnerID, &t.IsReadOnly, &t.ProviderEventID, &recurringID,
		&t.AccountID, &t.CalendarID, &t.ProviderCalendarID, &t.EmailLinkID,
		&faUserID, &emailAddress, &provider)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t.ProviderRecurringEventID = textPtr(recurringID)
	t.TokenIdentity = CalendarLinkTokenIdentity{
		ProviderUserID: faUserID.String,
		EmailAddress:   emailAddress,
		Provider:       provider,
	}
	owned, err := r.OwnedInboxEmails(ctx, requesterID)
	if err != nil {
		return nil, err
	}
	t.Actor = ActorInboxesFromOwned(owned)
	return &t, nil
}

// GetCreationTarget mirrors get_creation_target.
func (r *Repo) GetCreationTarget(ctx context.Context, requesterID string, emailLinkID, calendarID *uuid.UUID) (*CalendarCreationTarget, error) {
	var linkParam, calParam pgtype.UUID
	if emailLinkID != nil {
		linkParam = pgtype.UUID{Bytes: *emailLinkID, Valid: true}
	}
	if calendarID != nil {
		calParam = pgtype.UUID{Bytes: *calendarID, Valid: true}
	}
	var (
		t          CalendarCreationTarget
		accessRole pgtype.Text
		faUserID   pgtype.Text
		emailAddr  string
		provider   string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT
			link.macro_id AS owner_id,
			link.id AS email_link_id,
			account.id AS account_id,
			calendar.id AS calendar_id,
			calendar.provider_calendar_id,
			calendar.access_role,
			calendar.is_primary,
			link.fusionauth_user_id,
			link.email_address,
			link.provider::text AS provider
		FROM email_links link
		JOIN calendar_accounts account ON account.email_link_id = link.id
		JOIN calendars calendar ON calendar.account_id = account.id
		WHERE NOT calendar.is_deleted
		  AND account.sync_status <> 'disabled'
		  AND (
				($3::uuid IS NOT NULL AND calendar.id = $3)
				OR ($3::uuid IS NULL AND calendar.is_primary)
		  )
		  AND ($2::uuid IS NULL OR link.id = $2)
		  AND (
				link.macro_id = $1
				OR EXISTS (
					SELECT 1
					FROM macro_user_links delegation
					WHERE delegation.link_id = link.id
					  AND delegation.primary_macro_id = $1
				)
		  )
		ORDER BY
			(link.macro_id = $1) DESC,
			link.is_primary DESC,
			link.created_at ASC
		LIMIT 1`,
		requesterID, linkParam, calParam).Scan(
		&t.OwnerID, &t.EmailLinkID, &t.AccountID, &t.CalendarID,
		&t.ProviderCalendarID, &accessRole, &t.IsPrimary,
		&faUserID, &emailAddr, &provider)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t.IsReadOnly = !(accessRole.Valid && (accessRole.String == "owner" || accessRole.String == "writer"))
	t.TokenIdentity = CalendarLinkTokenIdentity{
		ProviderUserID: faUserID.String,
		EmailAddress:   emailAddr,
		Provider:       provider,
	}
	owned, err := r.OwnedInboxEmails(ctx, requesterID)
	if err != nil {
		return nil, err
	}
	t.Actor = ActorInboxesFromOwned(owned)
	return &t, nil
}

// OwnedInboxEmails mirrors owned_inbox_emails.
func (r *Repo) OwnedInboxEmails(ctx context.Context, requesterID string) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT email_address::text FROM email_links WHERE macro_id = $1`, requesterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEventAttendees mirrors get_event_attendees.
func (r *Repo) GetEventAttendees(ctx context.Context, eventID uuid.UUID) ([]CalendarAttendee, error) {
	m, err := fetchAttendees(ctx, r.pool, []uuid.UUID{eventID})
	if err != nil {
		return nil, err
	}
	return m[eventID], nil
}

// GetOccurrenceOverrideAttendees mirrors get_occurrence_override_attendees.
func (r *Repo) GetOccurrenceOverrideAttendees(ctx context.Context, eventID uuid.UUID, recurrenceID string) (*[]CalendarAttendee, error) {
	m, err := fetchOverrideAttendees(ctx, r.pool, []uuid.UUID{eventID})
	if err != nil {
		return nil, err
	}
	if a, ok := m[overrideKey{eventID, recurrenceID}]; ok {
		return &a, nil
	}
	return nil, nil
}

// FindWatchTarget mirrors find_watch_target.
func (r *Repo) FindWatchTarget(ctx context.Context, channelID, resourceID string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT account.email_link_id
		FROM calendars calendar
		JOIN calendar_accounts account ON account.id = calendar.account_id
		WHERE calendar.watch_channel_id = $1
		  AND calendar.watch_resource_id = $2
		  AND NOT calendar.is_deleted
		  AND account.sync_status <> 'disabled'`,
		channelID, resourceID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &id, nil
}

// ScheduleGoogleSyncForLink mirrors schedule_google_sync_for_link.
func (r *Repo) ScheduleGoogleSyncForLink(ctx context.Context, emailLinkID uuid.UUID) (bool, error) {
	var scheduled bool
	err := r.pool.QueryRow(ctx, `
		WITH due AS (
			UPDATE calendar_backfill_jobs job
			SET status = 'pending',
				cursor = '{}',
				last_error = NULL,
				started_at = NULL,
				completed_at = NULL,
				lease_token = NULL,
				lease_expires_at = NULL,
				updated_at = now()
			FROM calendar_accounts account, email_link_google_scopes scopes
			WHERE job.email_link_id = $1
			  AND job.email_link_id = account.email_link_id
			  AND job.account_id = account.id
			  AND scopes.link_id = job.email_link_id
			  AND job.kind = 'google_calendar'
			  AND job.status = 'complete'
			  AND job.grant_version = scopes.grant_version
			  AND scopes.granted_scopes @> $2::text[]
			  AND account.sync_status = 'ready'
			RETURNING job.id
		),
		republished AS (
			UPDATE calendar_sync_outbox outbox
			SET published_at = NULL
			FROM due
			WHERE outbox.backfill_job_id = due.id
		)
		SELECT count(*) > 0 FROM due`,
		emailLinkID, googleCalendarScopes).Scan(&scheduled)
	return scheduled, err
}

// ---------------------------------------------------------------------------
// batch fetch helpers
// ---------------------------------------------------------------------------

// fetchAttendees mirrors fetch_attendees.
func fetchAttendees(ctx context.Context, pool *pgxpool.Pool, eventIDs []uuid.UUID) (map[uuid.UUID][]CalendarAttendee, error) {
	out := map[uuid.UUID][]CalendarAttendee{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT
			event_id, email, display_name, response_status,
			is_organizer, is_optional, is_self, comment
		FROM calendar_event_attendees
		WHERE event_id = ANY($1::uuid[])
		ORDER BY event_id, email`,
		uuidStrs(eventIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eventID uuid.UUID
			a       CalendarAttendee
			name    pgtype.Text
			status  string
			comment pgtype.Text
		)
		if err := rows.Scan(&eventID, &a.Email, &name, &status,
			&a.IsOrganizer, &a.IsOptional, &a.IsSelf, &comment); err != nil {
			return nil, err
		}
		a.DisplayName = textPtr(name)
		a.ResponseStatus = attendeeStatusOf(status)
		a.Comment = textPtr(comment)
		out[eventID] = append(out[eventID], a)
	}
	return out, rows.Err()
}

type overrideKey struct {
	eventID      uuid.UUID
	recurrenceID string
}

// fetchOverrideAttendees mirrors fetch_override_attendees: per-occurrence
// attendee overrides keyed by (event_id, recurrence_id).
func fetchOverrideAttendees(ctx context.Context, pool *pgxpool.Pool, eventIDs []uuid.UUID) (map[overrideKey][]CalendarAttendee, error) {
	out := map[overrideKey][]CalendarAttendee{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT
			override.event_id,
			override.recurrence_id,
			attendee.email,
			attendee.display_name,
			attendee.response_status,
			attendee.is_organizer,
			attendee.is_optional,
			attendee.is_self,
			attendee.comment
		FROM calendar_event_overrides override
		LEFT JOIN calendar_event_override_attendees attendee
			ON attendee.event_id = override.event_id
		   AND attendee.recurrence_id = override.recurrence_id
		WHERE override.event_id = ANY($1::uuid[])
		  AND override.attendees_overridden
		ORDER BY override.event_id, override.recurrence_id, attendee.email`,
		uuidStrs(eventIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eventID      uuid.UUID
			recurrenceID string
			email        pgtype.Text
			name         pgtype.Text
			status       pgtype.Text
			isOrganizer  pgtype.Bool
			isOptional   pgtype.Bool
			isSelf       pgtype.Bool
			comment      pgtype.Text
		)
		if err := rows.Scan(&eventID, &recurrenceID, &email, &name, &status,
			&isOrganizer, &isOptional, &isSelf, &comment); err != nil {
			return nil, err
		}
		k := overrideKey{eventID, recurrenceID}
		if _, ok := out[k]; !ok {
			out[k] = []CalendarAttendee{}
		}
		if email.Valid && status.Valid {
			out[k] = append(out[k], CalendarAttendee{
				Email:          email.String,
				DisplayName:    textPtr(name),
				ResponseStatus: attendeeStatusOf(status.String),
				IsOrganizer:    isOrganizer.Bool,
				IsOptional:     isOptional.Bool,
				IsSelf:         isSelf.Bool,
				Comment:        textPtr(comment),
			})
		}
	}
	return out, rows.Err()
}

// fetchSourceContents mirrors fetch_source_contents.
func fetchSourceContents(ctx context.Context, pool *pgxpool.Pool, eventIDs []uuid.UUID) (map[uuid.UUID][]CalendarEventSourceContent, error) {
	out := map[uuid.UUID][]CalendarEventSourceContent{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT
			source.event_id,
			source.calendar_id,
			source.title,
			source.description,
			source.location,
			source.event_type,
			source.visibility,
			source.transparency,
			source.is_read_only,
			source.reminders_use_default,
			source.reminder_overrides,
			source.creator_email,
			source.creator_name
		FROM calendar_event_sources source
		JOIN calendars calendar ON calendar.id = source.calendar_id
		JOIN calendar_accounts account ON account.id = source.account_id
		WHERE source.event_id = ANY($1::uuid[])
		  AND NOT calendar.is_deleted
		  AND account.sync_status <> 'disabled'
		ORDER BY
			source.event_id,
			calendar.is_primary DESC,
			(calendar.access_role IN ('owner', 'writer')) DESC NULLS LAST,
			source.source_sequence DESC,
			source.source_updated_at DESC,
			source.id DESC`,
		uuidStrs(eventIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eventID       uuid.UUID
			c             CalendarEventSourceContent
			desc          pgtype.Text
			loc           pgtype.Text
			etype         string
			vis           string
			trans         string
			useDefault    bool
			overridesJSON []byte
			creatorEmail  pgtype.Text
			creatorName   pgtype.Text
		)
		if err := rows.Scan(&eventID, &c.CalendarID, &c.Title, &desc, &loc,
			&etype, &vis, &trans, &c.IsReadOnly, &useDefault, &overridesJSON,
			&creatorEmail, &creatorName); err != nil {
			return nil, err
		}
		c.Description = textPtr(desc)
		c.Location = textPtr(loc)
		c.EventType = eventTypeOf(etype)
		c.Visibility = eventVisibilityOf(vis)
		c.Transparency = eventTransparencyOf(trans)
		c.Reminders = EventReminders{UseDefault: useDefault}
		if len(overridesJSON) > 0 {
			if err := json.Unmarshal(overridesJSON, &c.Reminders.Overrides); err != nil {
				slog.Error("calendar: malformed source reminder_overrides json",
					"event_id", eventID, "calendar_id", c.CalendarID, "err", err)
			}
		}
		c.CreatorEmail = textPtr(creatorEmail)
		c.CreatorName = textPtr(creatorName)
		out[eventID] = append(out[eventID], c)
	}
	return out, rows.Err()
}
