-- name: CreateExperimentLog :one
INSERT INTO "ExperimentLog" (experiment_id, user_id, "group")
            VALUES ($1, $2, $3)
            RETURNING experiment_id, user_id, "group" as experiment_group, completed;


-- name: CompleteExperimentLog :exec
UPDATE "ExperimentLog"
            SET completed = true
            WHERE user_id = $1 AND experiment_id = $2;


-- name: BulkCreateExperimentLogs :exec
INSERT INTO "ExperimentLog" (experiment_id, user_id, "group")
                VALUES ($1, $2, $3);

