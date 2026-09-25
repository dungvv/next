// Package notifdb contains sqlc-generated queries for the notification
// database (user notifications, device registrations, unsubscribes, email-sent
// tracking, mute list). It is the Go port of crates/notification_db_client.
// Queries are defined in sqlc/notifdb/*.sql; regenerate with `sqlc generate`
// from the repo root.
//
// Notes for callers porting the Rust call sites:
//   - Rust lowercased emails in-code before calling
//     Upsert/RemoveEmailUnsubscribe and UpsertNotificationEmailUnsubscribeCode;
//     Go callers must do strings.ToLower themselves.
//   - Rust generated uuid v7 ids in-code (UpsertUserDevice,
//     UpsertNotificationEmailUnsubscribeCode); Go callers do the same.
//   - Queries declared :one that used fetch_optional in Rust return
//     pgx.ErrNoRows on absence.
package notifdb
