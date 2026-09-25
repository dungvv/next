-- Queries ported from crates/notification_db_client/src/email_unsubscribe_code.rs
-- Callers must lowercase the email before calling (Rust did it in-code) and
-- generate the uuid v7 code themselves.

-- name: UpsertNotificationEmailUnsubscribeCode :one
-- On conflict the existing code is kept and returned.
INSERT INTO notification_email_unsubscribe_code (email, code) VALUES ($1, $2)
ON CONFLICT (email) DO UPDATE
SET code = notification_email_unsubscribe_code.code
RETURNING notification_email_unsubscribe_code.code;

-- name: GetEmailByCode :one
-- Rust used fetch_optional: callers must treat pgx.ErrNoRows as "not found".
SELECT email FROM notification_email_unsubscribe_code WHERE code = $1;
