-- name: GetCalendarEventsForSearch :many
SELECT
            event.id AS "id",
            event.owner_id AS "owner_id",
            event.title AS "title",
            event.status AS "status",
            event.starts_at,
            event.ends_at,
            event.start_date,
            event.end_date,
            event.time_zone,
            (cardinality(event.recurrence_lines) > 0) AS "is_recurring",
            event.conference_url,
            event.organizer_email,
            event.organizer_name,
            event.description,
            event.is_read_only AS "is_read_only",
            event.created_at AS "created_at",
            event.updated_at AS "updated_at",
            occurrence.occurrence_key AS "occurrence_key",
            occurrence.starts_at AS "occurrence_starts_at",
            occurrence.ends_at AS "occurrence_ends_at",
            occurrence.start_date AS "occurrence_start_date",
            occurrence.end_date AS "occurrence_end_date"
        FROM calendar_events event
        LEFT JOIN LATERAL (
            SELECT
                instance.occurrence_key,
                instance.starts_at,
                instance.ends_at,
                instance.start_date,
                instance.end_date
            FROM calendar_event_occurrences instance
            WHERE instance.event_id = event.id
              AND NOT instance.is_cancelled
            ORDER BY
                (COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') >= $3) DESC,
                CASE WHEN COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') >= $3
                    THEN COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') END ASC,
                COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') DESC,
                instance.occurrence_key
            LIMIT 1
        ) occurrence ON true
        WHERE event."id" = ANY($2::uuid[])
          AND (
                event.owner_id = $1
                OR EXISTS (
                    SELECT 1
                    FROM macro_user_links link
                    WHERE link.link_id = event.source_link_id
                      AND link.primary_macro_id = $1
                )
          );

