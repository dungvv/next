-- name: GetAccessibleCallIds :many
WITH user_source_ids AS (
            SELECT cp.channel_id::text AS source_id
            FROM comms_channel_participants cp
            WHERE cp.user_id = $1 AND cp.left_at IS NULL
            UNION ALL
            SELECT t.team_id::text
            FROM team_user t
            WHERE t.user_id = $1
            UNION ALL
            SELECT $1
        ),
        visible_calls AS (
            SELECT
                cr.id,
                CASE
                    WHEN EXISTS (
                        SELECT 1 FROM call_record_participants crp
                        WHERE crp.call_record_id = cr.id AND crp.user_id = $1
                    ) THEN 'ATTENDED'
                    WHEN EXISTS (
                        SELECT 1 FROM comms_channel_participants ccp
                        WHERE ccp.channel_id = cr.channel_id
                          AND ccp.user_id = $1
                          AND ccp.left_at IS NULL
                    ) THEN 'MISSED'
                    ELSE 'UNATTENDED'
                END AS status
            FROM call_records cr
            WHERE (
                EXISTS (
                    SELECT 1 FROM entity_access ea
                    WHERE ea.entity_id = cr.id
                      AND ea.entity_type = 'call'
                      AND ea.source_id IN (SELECT source_id FROM user_source_ids)
                ) OR EXISTS (
                    SELECT 1 FROM "SharePermission" sp
                    WHERE sp.id = cr.share_permission_id
                      AND sp."linkShareAccessLevel" IS NOT NULL
                      AND (
                          sp."linkShare" = 'PUBLIC'
                          OR (
                              sp."linkShare" = 'TEAM'
                              AND EXISTS (
                                  SELECT 1
                                  FROM team_user owner_team
                                  WHERE owner_team.user_id = cr.created_by
                                    AND owner_team.team_id::text IN (
                                        SELECT source_id FROM user_source_ids
                                    )
                              )
                          )
                      )
                )
            )
        )
        SELECT id AS "id"
        FROM visible_calls
        WHERE cardinality($2::text[]) = 0 OR "status" = ANY($2::text[]);


-- name: GetCallRecordsForSearchBackfill :many
SELECT
            id AS "call_id",
            started_at AS "started_at"
        FROM call_records
        WHERE
            ($2::timestamptz IS NULL OR started_at >= $2)
            AND ($3::timestamptz IS NULL OR started_at < $3)
            AND (
                $4::timestamptz IS NULL
                OR (started_at, id) > ($4, $5::uuid)
            )
        ORDER BY started_at ASC, id ASC
        LIMIT $1;


-- name: GetCallRecordSearchPayload :one
SELECT
            cr.id AS "call_id",
            cr.channel_id AS "channel_id",
            cr.created_by AS "created_by",
            cr.custom_name AS "custom_name",
            cc.name AS "channel_name"
        FROM call_records cr
        LEFT JOIN comms_channels cc ON cc.id = cr.channel_id
        WHERE cr.id = $1;


-- name: GetCallRecordSearchPayload2 :many
SELECT user_id AS "user_id"
        FROM call_record_participants
        WHERE call_record_id = $1
        ORDER BY joined_at ASC;


-- name: GetCallRecordSearchPayload3 :many
SELECT
            id AS "transcript_id",
            speaker_id AS "speaker_id",
            sequence_num AS "sequence_num",
            content AS "content",
            started_at AS "started_at",
            ended_at
        FROM call_record_transcripts
        WHERE call_record_id = $1
        ORDER BY sequence_num ASC;


-- name: GetCallRecordsMetadata :many
SELECT
            cr.id AS "call_id",
            cr.channel_id AS "channel_id",
            cr.created_by AS "created_by",
            cr.started_at AS "started_at",
            cr.ended_at AS "ended_at",
            cr.duration_ms AS "duration_ms",
            cr.custom_name AS "custom_name",
            EXISTS (
                SELECT 1 FROM call_record_participants crp
                WHERE crp.call_record_id = cr.id AND crp.user_id = $2
            ) AS "attended",
            CASE
                WHEN EXISTS (
                    SELECT 1 FROM call_record_participants crp
                    WHERE crp.call_record_id = cr.id AND crp.user_id = $2
                ) THEN 'ATTENDED'
                WHEN EXISTS (
                    SELECT 1 FROM comms_channel_participants ccp
                    WHERE ccp.channel_id = cr.channel_id
                      AND ccp.user_id = $2
                      AND ccp.left_at IS NULL
                ) THEN 'MISSED'
                ELSE 'UNATTENDED'
            END AS "status"
        FROM call_records cr
        WHERE cr."id" = ANY($1::uuid[]);

