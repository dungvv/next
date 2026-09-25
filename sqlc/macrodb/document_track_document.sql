-- name: TrackDocument :exec
INSERT INTO "DocumentView" ("document_id", "user_id", "created_at")
        VALUES ($1, $2, NOW());

