-- name: PromoteLinkToShared :exec
INSERT INTO macro_user (id, username, email, stripe_customer_id)
        VALUES ($1, $2, $3, $4);


-- name: PromoteLinkToShared2 :exec
INSERT INTO macro_user_email_verification (macro_user_id, email, is_verified)
        VALUES ($1, $2, true);


-- name: PromoteLinkToShared3 :exec
INSERT INTO "User" (id, email, macro_user_id, "organizationId")
        VALUES ($1, $2, $3, $4);


-- name: PromoteLinkToShared4 :exec
INSERT INTO promoted_shared_mailboxes (macro_id)
        VALUES ($1)
        ON CONFLICT (macro_id) DO NOTHING;


-- name: PromoteLinkToShared5 :exec
UPDATE email_links
        SET macro_id = $1, email_address = $2, updated_at = NOW()
        WHERE id = $3;


-- name: IsPromotedSharedMailbox :one
SELECT EXISTS(
            SELECT 1 FROM promoted_shared_mailboxes WHERE macro_id = $1
        ) AS "exists";


-- name: DeletePromotedMailboxUser :one
DELETE FROM "User"
        WHERE id = $1
          AND EXISTS (SELECT 1 FROM promoted_shared_mailboxes WHERE macro_id = $1)
        RETURNING macro_user_id;


-- name: DeletePromotedMailboxUser2 :exec
DELETE FROM macro_user WHERE id = $1;

