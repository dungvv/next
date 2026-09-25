-- name: GetLegacyUserInfo :one
SELECT
            u."id" as "user_id",
            u."email" as "email",
            u."stripeCustomerId" as "stripe_customer_id",
            u."name" as name,
            u."tutorialComplete" as tutorial_complete,
            u."group" as "group",
            u."hasChromeExt" as has_chrome_ext,
            u."aiDataConsent" as ai_data_consent,
            mu.has_trialed as has_trialed,
            u.macro_user_id as "macro_user_id",
            u."createdAt" as "created_at"
        FROM "User" u
        JOIN "macro_user" mu ON u.macro_user_id = mu.id
        WHERE u."id" = $1;

