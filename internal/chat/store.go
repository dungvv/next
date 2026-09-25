// Chat store: pgx queries over the comms_* tables. The generated commsdb
// sqlc package only covers ~26 queries, so the remaining access lives here.
package chat

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macro-inc/macro/pkg/store/commsdb"
)

// store wraps the comms pool plus the generated commsdb queries.
type store struct {
	pool *pgxpool.Pool
	q    *commsdb.Queries
}

func newStore(pool *pgxpool.Pool) *store {
	return &store{pool: pool, q: commsdb.New(pool)}
}

var errNotFound = errors.New("not found")
var errForbidden = errors.New("forbidden")

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgUUIDP(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*id)
}

type channelRow struct {
	ID               uuid.UUID
	Name             *string
	ChannelType      ChannelType
	OrgID            *int64
	TeamID           *uuid.UUID
	AutoJoinTeam     bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	OwnerID          string
	JoinCode         *uuid.UUID
	ProfilePictureID *uuid.UUID
}

func (s *store) getChannel(ctx context.Context, id uuid.UUID) (*channelRow, error) {
	var c channelRow
	var ct string
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, channel_type::text, org_id, team_id, auto_join_team,
		       created_at, updated_at, owner_id, join_code, profile_picture_id
		FROM comms_channels WHERE id = $1`, id).
		Scan(&c.ID, &c.Name, &ct, &c.OrgID, &c.TeamID, &c.AutoJoinTeam,
			&c.CreatedAt, &c.UpdatedAt, &c.OwnerID, &c.JoinCode, &c.ProfilePictureID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	c.ChannelType = ChannelType(ct)
	return &c, nil
}

func (s *store) insertChannel(ctx context.Context, c channelRow) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO comms_channels (id, name, channel_type, org_id, team_id,
		                            auto_join_team, owner_id)
		VALUES ($1, $2, $3::comms_channel_type, $4, $5, $6, $7)
		RETURNING id`,
		c.ID, c.Name, string(c.ChannelType), c.OrgID, c.TeamID, c.AutoJoinTeam, c.OwnerID).
		Scan(&id)
	return id, err
}

// addParticipant upserts membership; a previously-left row rejoins.
func (s *store) addParticipant(ctx context.Context, ex pgx.Tx, channelID uuid.UUID, userID string, role ParticipantRole) error {
	_, err := ex.Exec(ctx, `
		INSERT INTO comms_channel_participants (channel_id, user_id, role, joined_at, left_at)
		VALUES ($1, $2, $3::comms_participant_role, now(), NULL)
		ON CONFLICT (channel_id, user_id) DO UPDATE
		SET left_at = NULL, joined_at = now(), role = EXCLUDED.role`,
		channelID, userID, string(role))
	return err
}

func (s *store) removeParticipant(ctx context.Context, channelID uuid.UUID, userID string) error {
	res, err := s.pool.Exec(ctx, `
		UPDATE comms_channel_participants SET left_at = now()
		WHERE channel_id = $1 AND user_id = $2 AND left_at IS NULL`, channelID, userID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

func (s *store) isParticipant(ctx context.Context, channelID uuid.UUID, userID string) (bool, ParticipantRole, error) {
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT role::text FROM comms_channel_participants
		WHERE channel_id = $1 AND user_id = $2 AND left_at IS NULL`, channelID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, ParticipantRole(role), nil
}

func (s *store) participants(ctx context.Context, channelID uuid.UUID) ([]ChannelParticipant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id, user_id, role::text, joined_at, left_at
		FROM comms_channel_participants
		WHERE channel_id = $1 AND left_at IS NULL
		ORDER BY joined_at`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelParticipant{}
	for rows.Next() {
		var p ChannelParticipant
		var role string
		if err := rows.Scan(&p.ChannelID, &p.UserID, &role, &p.JoinedAt, &p.LeftAt); err != nil {
			return nil, err
		}
		p.Role = ParticipantRole(role)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) participantUserIDs(ctx context.Context, channelID uuid.UUID) ([]string, error) {
	return s.q.GetChannelParticipantUserIDs(ctx, pgUUID(channelID))
}

// threadParticipantUserIDs returns participants of the thread's channel plus
// anyone who has replied in the thread (Rust get_channel_participants_for_thread).
func (s *store) threadParticipantUserIDs(ctx context.Context, rootID uuid.UUID) ([]string, error) {
	return s.q.GetChannelParticipantsForThread(ctx, pgUUID(rootID))
}

func (s *store) findDM(ctx context.Context, a, b string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT c.id FROM comms_channels c
		WHERE c.channel_type = 'direct_message'
		  AND (SELECT count(*) FROM comms_channel_participants p
		       WHERE p.channel_id = c.id AND p.left_at IS NULL) = 2
		  AND EXISTS (SELECT 1 FROM comms_channel_participants p
		              WHERE p.channel_id = c.id AND p.user_id = $1 AND p.left_at IS NULL)
		  AND EXISTS (SELECT 1 FROM comms_channel_participants p
		              WHERE p.channel_id = c.id AND p.user_id = $2 AND p.left_at IS NULL)
		LIMIT 1`, a, b).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

// findPrivate returns a private channel whose active participant set is
// exactly members ∪ {owner}.
func (s *store) findPrivate(ctx context.Context, members []string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT c.id FROM comms_channels c
		WHERE c.channel_type IN ('private', 'team')
		  AND NOT EXISTS (
			SELECT 1 FROM comms_channel_participants p
			WHERE p.channel_id = c.id AND p.left_at IS NULL
			  AND p.user_id <> ALL($1::text[]))
		  AND NOT EXISTS (
			SELECT 1 FROM unnest($1::text[]) u
			WHERE NOT EXISTS (
			  SELECT 1 FROM comms_channel_participants p
			  WHERE p.channel_id = c.id AND p.user_id = u AND p.left_at IS NULL))
		LIMIT 1`, members).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

func (s *store) patchChannel(ctx context.Context, id uuid.UUID, name *string, channelType *ChannelType, teamID *uuid.UUID, autoJoin *bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE comms_channels SET
			name = COALESCE($2, name),
			channel_type = COALESCE($3::comms_channel_type, channel_type),
			team_id = CASE WHEN $4 THEN team_id ELSE team_id END,
			auto_join_team = COALESCE($5, auto_join_team),
			updated_at = now()
		WHERE id = $1`, id, name, channelTypeString(channelType), autoJoin != nil, autoJoin)
	return err
}

func channelTypeString(t *ChannelType) *string {
	if t == nil {
		return nil
	}
	s := string(*t)
	return &s
}

func (s *store) setTeamID(ctx context.Context, id uuid.UUID, teamID *uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE comms_channels SET team_id = $2, updated_at = now() WHERE id = $1`, id, teamID)
	return err
}

// deleteChannel hard-deletes the channel and its comms data in one tx; the
// jobs.delete_chat consumer handles cross-entity cleanup.
func (s *store) deleteChannel(ctx context.Context, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	stmts := []string{
		`DELETE FROM comms_reactions WHERE message_id IN (SELECT id FROM comms_messages WHERE channel_id = $1)`,
		`DELETE FROM comms_attachments WHERE channel_id = $1`,
		`DELETE FROM comms_entity_mentions WHERE source_entity_type = 'channel' AND source_entity_id = $1::text`,
		`DELETE FROM comms_messages WHERE channel_id = $1`,
		`DELETE FROM comms_activity WHERE channel_id = $1`,
		`DELETE FROM comms_channel_participants WHERE channel_id = $1`,
		`DELETE FROM comms_channels WHERE id = $1`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// getOrCreateJoinCode returns the channel's reusable join code, minting one
// for private/team channels that don't have it yet.
func (s *store) getOrCreateJoinCode(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var code uuid.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE comms_channels SET join_code = gen_random_uuid()
		WHERE id = $1 AND join_code IS NULL
		RETURNING join_code`, id).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT join_code FROM comms_channels WHERE id = $1`, id).Scan(&code)
	}
	return code, err
}

func (s *store) channelByJoinCode(ctx context.Context, code uuid.UUID) (*channelRow, error) {
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT id FROM comms_channels WHERE join_code = $1`, code).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	return s.getChannel(ctx, id)
}

func (s *store) setProfilePicture(ctx context.Context, id uuid.UUID, picID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE comms_channels SET profile_picture_id = $2, updated_at = now() WHERE id = $1`, id, picID)
	return err
}

func (s *store) touchChannel(ctx context.Context, ex pgx.Tx, id uuid.UUID) error {
	var err error
	if ex != nil {
		_, err = ex.Exec(ctx, `UPDATE comms_channels SET updated_at = now() WHERE id = $1`, id)
	} else {
		_, err = s.pool.Exec(ctx, `UPDATE comms_channels SET updated_at = now() WHERE id = $1`, id)
	}
	return err
}

// ---- messages ----

type messageRow struct {
	ID          uuid.UUID
	ChannelID   uuid.UUID
	ThreadID    *uuid.UUID
	SenderID    string
	TriggeredBy *string
	Content     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	EditedAt    *time.Time
	DeletedAt   *time.Time
}

const messageCols = `id, channel_id, thread_id, sender_id, triggered_by, content,
	created_at, updated_at, edited_at, deleted_at`

func scanMessage(row pgx.Row) (*messageRow, error) {
	var m messageRow
	err := row.Scan(&m.ID, &m.ChannelID, &m.ThreadID, &m.SenderID, &m.TriggeredBy,
		&m.Content, &m.CreatedAt, &m.UpdatedAt, &m.EditedAt, &m.DeletedAt)
	return &m, err
}

func (s *store) createMessage(ctx context.Context, ex pgx.Tx, m *messageRow) error {
	var err error
	q := `
		INSERT INTO comms_messages (id, channel_id, thread_id, sender_id, triggered_by, content)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + messageCols
	var row pgx.Row
	if ex != nil {
		row = ex.QueryRow(ctx, q, m.ID, m.ChannelID, m.ThreadID, m.SenderID, m.TriggeredBy, m.Content)
	} else {
		row = s.pool.QueryRow(ctx, q, m.ID, m.ChannelID, m.ThreadID, m.SenderID, m.TriggeredBy, m.Content)
	}
	got, err := scanMessage(row)
	if err == nil {
		*m = *got
	}
	return err
}

func (s *store) getMessage(ctx context.Context, id uuid.UUID) (*messageRow, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx,
		`SELECT `+messageCols+` FROM comms_messages WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return m, err
}

// patchMessage updates content/edited_at; caller rewrites mentions/attachments.
func (s *store) patchMessage(ctx context.Context, id uuid.UUID, content *string) (*messageRow, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx, `
		UPDATE comms_messages SET
			content = COALESCE($2, content),
			edited_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+messageCols, id, content))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return m, err
}

func (s *store) softDeleteMessage(ctx context.Context, id uuid.UUID) (*messageRow, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx, `
		UPDATE comms_messages SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+messageCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return m, err
}

// listMessages returns channel messages (both top-level and replies) for the
// flat list endpoint; callers thread-shape the result.
func (s *store) listMessages(ctx context.Context, channelID uuid.UUID, before *time.Time, after *time.Time, limit int) ([]messageRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageCols+` FROM comms_messages
		WHERE channel_id = $1
		  AND ($2::timestamptz IS NULL OR created_at < $2)
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		ORDER BY created_at DESC
		LIMIT $4`, channelID, before, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []messageRow{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// threadReplies returns active replies for a thread root, oldest first.
func (s *store) threadReplies(ctx context.Context, rootID uuid.UUID, limit int) ([]messageRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageCols+` FROM comms_messages
		WHERE thread_id = $1 AND deleted_at IS NULL
		ORDER BY created_at ASC LIMIT $2`, rootID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []messageRow{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *store) countedReactions(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID][]CountedReaction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT message_id, emoji, array_agg(user_id ORDER BY created_at) AS users
		FROM comms_reactions
		WHERE message_id = ANY($1::uuid[])
		GROUP BY message_id, emoji`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]CountedReaction{}
	for rows.Next() {
		var mid uuid.UUID
		var r CountedReaction
		if err := rows.Scan(&mid, &r.Emoji, &r.Users); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], r)
	}
	return out, rows.Err()
}

func (s *store) addReaction(ctx context.Context, messageID uuid.UUID, emoji, userID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO comms_reactions (message_id, emoji, user_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (message_id, emoji, user_id) DO NOTHING`, messageID, emoji, userID)
	return err
}

func (s *store) removeReaction(ctx context.Context, messageID uuid.UUID, emoji, userID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM comms_reactions WHERE message_id = $1 AND emoji = $2 AND user_id = $3`,
		messageID, emoji, userID)
	return err
}

func (s *store) messageAttachments(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID][]MessageAttachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.message_id, a.entity_type, a.entity_id, a.width, a.height, a.created_at
		FROM comms_attachments a WHERE a.message_id = ANY($1::uuid[])`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]MessageAttachment{}
	for rows.Next() {
		var a MessageAttachment
		var mid uuid.UUID
		if err := rows.Scan(&a.ID, &mid, &a.EntityType, &a.EntityID, &a.Width, &a.Height, &a.CreatedAt); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], a)
	}
	return out, rows.Err()
}

func (s *store) addAttachments(ctx context.Context, ex pgx.Tx, channelID, messageID uuid.UUID, atts []NewChannelAttachment) ([]MessageAttachment, error) {
	const q = `INSERT INTO comms_attachments (id, channel_id, message_id, entity_type, entity_id, width, height)
	           VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)
	           RETURNING id, entity_type, entity_id, width, height, created_at`
	out := make([]MessageAttachment, 0, len(atts))
	for _, a := range atts {
		var ins MessageAttachment
		var row pgx.Row
		if ex != nil {
			row = ex.QueryRow(ctx, q, channelID, messageID, a.EntityType, a.EntityID, a.Width, a.Height)
		} else {
			row = s.pool.QueryRow(ctx, q, channelID, messageID, a.EntityType, a.EntityID, a.Width, a.Height)
		}
		if err := row.Scan(&ins.ID, &ins.EntityType, &ins.EntityID, &ins.Width, &ins.Height, &ins.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ins)
	}
	return out, nil
}

func (s *store) deleteAttachments(ctx context.Context, messageID uuid.UUID, ids []string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM comms_attachments WHERE message_id = $1 AND id = ANY($2::uuid[])`,
		messageID, ids)
	return err
}

func (s *store) channelAttachments(ctx context.Context, channelID uuid.UUID, limit int) ([]ChannelAttachment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.channel_id, a.message_id, m.sender_id, a.entity_type, a.entity_id,
		       a.width, a.height, a.created_at
		FROM comms_attachments a
		JOIN comms_messages m ON m.id = a.message_id
		WHERE a.channel_id = $1 AND m.deleted_at IS NULL
		ORDER BY a.created_at DESC LIMIT $2`, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelAttachment{}
	for rows.Next() {
		var a ChannelAttachment
		if err := rows.Scan(&a.ID, &a.ChannelID, &a.MessageID, &a.SenderID,
			&a.EntityType, &a.EntityID, &a.Width, &a.Height, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *store) threadInfo(ctx context.Context, rootID uuid.UUID) (count int64, latest *time.Time, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*), max(created_at) FROM comms_messages
		WHERE thread_id = $1 AND deleted_at IS NULL`, rootID).Scan(&count, &latest)
	return count, latest, err
}

// messageMentions returns "entity_type:entity_id" mention strings.
func (s *store) messageMentions(ctx context.Context, messageID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT entity_type || ':' || entity_id FROM comms_entity_mentions
		WHERE source_entity_type = 'message' AND source_entity_id = $1::text`, messageID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- activity ----

// upsertActivity records a view or interaction for the user on the channel.
func (s *store) upsertActivity(ctx context.Context, userID string, channelID uuid.UUID, kind string) error {
	col := "viewed_at"
	if kind == "interact" {
		col = "interacted_at"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO comms_activity (id, user_id, channel_id, `+col+`)
		VALUES (gen_random_uuid(), $1, $2, now())
		ON CONFLICT (user_id, channel_id) DO UPDATE
		SET `+col+` = now(), updated_at = now()`, userID, channelID)
	return err
}

// upsertActivityBestEffort is upsertActivity that logs instead of failing —
// used in post-commit side-effect paths.
func (s *store) upsertActivityBestEffort(ctx context.Context, userID string, channelID uuid.UUID, kind string) {
	if err := s.upsertActivity(ctx, userID, channelID, kind); err != nil {
		slog.WarnContext(ctx, "chat: activity upsert failed", "channel_id", channelID, "err", err)
	}
}

// userActivity returns the caller's activity rows (channel_id + timestamps).
func (s *store) userActivity(ctx context.Context, userID string) ([]UserActivityRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id, viewed_at, interacted_at, created_at, updated_at
		FROM comms_activity WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserActivityRow{}
	for rows.Next() {
		var a UserActivityRow
		if err := rows.Scan(&a.ChannelID, &a.ViewedAt, &a.InteractedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// userChannels lists the caller's channels ordered by updated_at DESC with
// keyset pagination (Rust paginate_on SimpleSortMethod::UpdatedAt): the
// cursor is the last row's (updated_at, id).
func (s *store) userChannels(ctx context.Context, userID string, limit int, before *time.Time, beforeID *uuid.UUID) ([]channelRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.name, c.channel_type::text, c.org_id, c.team_id,
		       c.auto_join_team, c.created_at, c.updated_at, c.owner_id,
		       c.join_code, c.profile_picture_id
		FROM comms_channels c
		JOIN comms_channel_participants p ON p.channel_id = c.id
		WHERE p.user_id = $1 AND p.left_at IS NULL
		  AND ($2::timestamptz IS NULL OR
		       (c.updated_at, c.id) < ($2, $3::uuid))
		ORDER BY c.updated_at DESC, c.id DESC
		LIMIT $4`, userID, before, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []channelRow{}
	for rows.Next() {
		var c channelRow
		var ct string
		if err := rows.Scan(&c.ID, &c.Name, &ct, &c.OrgID, &c.TeamID, &c.AutoJoinTeam,
			&c.CreatedAt, &c.UpdatedAt, &c.OwnerID, &c.JoinCode, &c.ProfilePictureID); err != nil {
			return nil, err
		}
		c.ChannelType = ChannelType(ct)
		out = append(out, c)
	}
	return out, rows.Err()
}

// userNames resolves display names for participant ids (ports
// get_names_for_ids: macro_user_info ⋈ "User" on macro_user_id).
func (s *store) userNames(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, mui.first_name, mui.last_name
		FROM macro_user_info mui
		JOIN "User" u ON mui.macro_user_id = u.macro_user_id
		WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var first, last *string
		if err := rows.Scan(&id, &first, &last); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(strings.Join([]string{deref(first), deref(last)}, " "))
		if name != "" {
			out[id] = name
		}
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// latestMessages returns the latest message and latest top-level message of
// a channel; mentions are formatted `entity_type:entity_id` per the Rust
// ApiChannelListMessage contract.
func (s *store) latestMessages(ctx context.Context, channelID uuid.UUID) (latest, latestTop *RecentChannelMessage, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.thread_id, m.sender_id, m.content, m.created_at,
		       m.updated_at, m.deleted_at,
		       COALESCE(array_agg(em.entity_type || ':' || em.entity_id)
		                FILTER (WHERE em.id IS NOT NULL), '{}')
		FROM comms_messages m
		LEFT JOIN comms_entity_mentions em
		  ON em.source_entity_type = 'message' AND em.source_entity_id = m.id::text
		WHERE m.channel_id = $1 AND m.deleted_at IS NULL
		GROUP BY m.id
		ORDER BY m.created_at DESC LIMIT 20`, channelID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m RecentChannelMessage
		if err := rows.Scan(&m.MessageID, &m.ThreadID, &m.SenderID, &m.Content,
			&m.CreatedAt, &m.UpdatedAt, &m.DeletedAt, &m.Mentions); err != nil {
			return nil, nil, err
		}
		if latest == nil {
			cp := m
			latest = &cp
		}
		if latestTop == nil && m.ThreadID == nil {
			cp := m
			latestTop = &cp
		}
		if latest != nil && latestTop != nil {
			break
		}
	}
	return latest, latestTop, rows.Err()
}

func (s *store) activityFor(ctx context.Context, userID string, channelID uuid.UUID) (viewed, interacted *time.Time, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT viewed_at, interacted_at FROM comms_activity
		WHERE user_id = $1 AND channel_id = $2`, userID, channelID).Scan(&viewed, &interacted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return viewed, interacted, err
}

// ---- mentions ----

func (s *store) createEntityMention(ctx context.Context, req CreateEntityMentionRequest, userID *string) (*EntityMention, error) {
	var m EntityMention
	err := s.pool.QueryRow(ctx, `
		INSERT INTO comms_entity_mentions
			(source_entity_type, source_entity_id, entity_type, entity_id, user_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, source_entity_type, source_entity_id, entity_type, entity_id, user_id, created_at`,
		req.SourceEntityType, req.SourceEntityID, req.EntityType, req.EntityID, userID).
		Scan(&m.ID, &m.SourceEntityType, &m.SourceEntityID, &m.EntityType, &m.EntityID, &m.UserID, &m.CreatedAt)
	return &m, err
}

func (s *store) deleteEntityMention(ctx context.Context, id uuid.UUID) (bool, error) {
	res, err := s.pool.Exec(ctx, `DELETE FROM comms_entity_mentions WHERE id = $1`, id)
	return res.RowsAffected() > 0, err
}

// attachmentReferences returns channels/messages that reference the entity,
// used by GET /channels/attachments/{entity_type}/{entity_id}/references.
func (s *store) attachmentReferences(ctx context.Context, entityType, entityID string) ([]ChannelAttachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.channel_id, a.message_id, m.sender_id, a.entity_type, a.entity_id,
		       a.width, a.height, a.created_at
		FROM comms_attachments a
		JOIN comms_messages m ON m.id = a.message_id
		WHERE a.entity_type = $1 AND a.entity_id = $2
		ORDER BY a.created_at DESC`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelAttachment{}
	for rows.Next() {
		var a ChannelAttachment
		if err := rows.Scan(&a.ID, &a.ChannelID, &a.MessageID, &a.SenderID,
			&a.EntityType, &a.EntityID, &a.Width, &a.Height, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
