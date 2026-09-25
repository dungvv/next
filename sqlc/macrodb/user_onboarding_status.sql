-- name: GetOnboardingStatus :one
SELECT "hasOnboardingDocuments"::boolean
        FROM "User"
        WHERE id = $1;


-- name: SetOnboardingStatus :exec
UPDATE "User"
        SET "hasOnboardingDocuments" = $1
        WHERE id = $2;

