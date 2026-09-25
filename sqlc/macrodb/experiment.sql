-- name: CreateExperiment :one
INSERT INTO "Experiment" (id)
            VALUES ($1)
            RETURNING id, active, "started_at"::timestamptz as started_at, "ended_at"::timestamptz as ended_at;


-- name: PatchExperiment :exec
UPDATE "Experiment" SET active = true AND started_at = NOW() WHERE id = $1;


-- name: PatchExperiment2 :exec
UPDATE "Experiment" SET active = false AND ended_at = NOW() WHERE id = $1;


-- name: DeleteExperiment :exec
DELETE FROM "Experiment" WHERE id = $1;


-- name: GetActiveExperiments :many
SELECT e.id, e.active, e."started_at"::timestamptz as started_at, e."ended_at"::timestamptz as ended_at
            FROM "Experiment" e
            WHERE e.active = true;


-- name: GetActiveExperimentsForUser :many
SELECT e.id, e.active, e."started_at"::timestamptz as started_at, e."ended_at"::timestamptz as ended_at
            FROM "Experiment" e
            JOIN "ExperimentLog" el ON e.id = el.experiment_id
            WHERE el.user_id = $1 AND e.active = true;

