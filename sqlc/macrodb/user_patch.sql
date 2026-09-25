-- name: UpdateMacroUserHasTrialed :exec
UPDATE macro_user
        SET has_trialed = $2
        WHERE email = $1;


-- name: UpdateMacroUserId :exec
UPDATE "User"
            SET macro_user_id = $1
            WHERE id = $2;


-- name: PatchUserGroup :exec
UPDATE "User"
            SET "group" = $1
            WHERE id = $2;


-- name: PatchUserTutorial :exec
UPDATE "User"
            SET "tutorialComplete" = $1
            WHERE id = $2;


-- name: PatchAiConsent :exec
UPDATE "User"
            SET "aiDataConsent" = $1
            WHERE id = $2;


-- name: PatchUserOnboarding :exec
UPDATE "User"
            SET "firstName" = $1,
                "lastName" = $2,
                "title" = $3,
                "industry" = $4
            WHERE id = $5;

