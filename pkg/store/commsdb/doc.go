// Package commsdb contains sqlc-generated queries for the comms database
// (channels, messages, participants, activity, entity mentions, attachments).
// It is the Go port of crates/comms_db_client. Queries are defined in
// sqlc/commsdb/*.sql; regenerate with `sqlc generate` from the repo root.
//
// Notes for callers porting the Rust call sites:
//   - Queries declared :one (GetActivityForChannel, GetDeviceEndpoint-style
//     lookups) return pgx.ErrNoRows where the Rust code used fetch_optional.
//   - Rust asserted some columns non-null (e.g. comms_messages.channel_id);
//     the schema has since made them nullable, so generated fields are
//     pgtype.UUID / pgtype.Text — check .Valid.
//   - create_channel/seed_channel are covered by InsertChannel; both message
//     insert variants are covered by CreateMessage; the owner/member inserts
//     inside channel creation use AddChannelParticipant.
package commsdb
