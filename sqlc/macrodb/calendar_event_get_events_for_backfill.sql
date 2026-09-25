-- name: GetCalendarEventsForSearchBackfill :many
SELECT
            event.id AS "event_id",
            event.updated_at AS "updated_at"
        FROM calendar_events event
        WHERE ($2::timestamptz IS NULL OR $3::uuid IS NULL
               OR (event.updated_at, event.id) > ($2, $3))
          AND ($4::timestamptz IS NULL OR event.updated_at >= $4)
          AND ($5::timestamptz IS NULL OR event.updated_at < $5)
        ORDER BY event.updated_at ASC, event.id ASC
        LIMIT $1;

