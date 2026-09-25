-- name: GetCalendarEventForIndex :one
SELECT
            event.id AS "id",
            event.owner_id AS "owner_id",
            event.source_link_id AS "source_link_id",
            event.ical_uid AS "ical_uid",
            event.title AS "title",
            event.status AS "status",
            event.starts_at,
            event.ends_at,
            event.start_date,
            event.end_date,
            (cardinality(event.recurrence_lines) > 0) AS "is_recurring",
            event.organizer_email,
            event.created_at AS "created_at",
            event.updated_at AS "updated_at",
            COALESCE(
                (
                    SELECT array_agg(lower(attendee.email) ORDER BY lower(attendee.email))
                    FROM calendar_event_attendees attendee
                    WHERE attendee.event_id = event.id
                ),
                '{}'
            )::text[] AS "attendee_emails",
            COALESCE(
                (
                    SELECT array_agg(DISTINCT source.title ORDER BY source.title)
                    FROM calendar_event_sources source
                    WHERE source.event_id = event.id
                      AND source.title <> ''
                ),
                '{}'
            )::text[] AS "source_titles"
        FROM calendar_events event
        WHERE event.id = $1;

