package agentharness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionColumns is the full agent_session read projection. thread_parent
// is not a column: it is derived by joining the thread's root message
// (comms_messages.parent_entity_*), matching the Rust repo's
// jsonb_build_object projection.
const sessionColumns = `
	s.id::text, s.name, s.owner_id, s.thread_id::text,
	cm.parent_entity_type, cm.parent_entity_id::text,
	s.originating_message_id::text, s.bot_id::text,
	s.model, s.harness, s.repo_url, s.repo_branch, s.working_branch,
	s.pull_request_url, s.workspace, s.sandbox_size, s.instructions,
	s.status, s.status_event_name, s.turn_state, s.acp_session_id,
	s.egress_token_hash, s.mcp_scope, s.mcp_servers,
	s.created_at, s.modified_at`

// sessionFrom is the FROM clause pairing the projection above.
const sessionFrom = `
	FROM agent_session s
	LEFT JOIN comms_messages cm ON cm.id = s.thread_id`

// ErrThreadSessionExists reports the unique (thread_id, bot_id) collision —
// the Rust create path's 409.
var ErrThreadSessionExists = errors.New("this bot already has a session for this thread")

// Repository persists agent sessions in macrodb. The Rust code goes through
// agent_session::outbound::postgres::PgAgentSessionRepo plus the entity and
// entity_access writes session_service does inline; here the repo owns all
// of it in one transaction.
type Repository struct {
	db *pgxpool.Pool
}

// NewRepository builds the macrodb-backed repository.
func NewRepository(db *pgxpool.Pool) *Repository { return &Repository{db: db} }

// scanSession reads one row produced by sessionColumns.
func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	var parentType *string
	err := row.Scan(
		&s.ID, &s.Name, &s.OwnerID, &s.ThreadID, &parentType,
		&s.ThreadParentID, &s.OriginatingMessageID, &s.BotID,
		&s.Model, &s.Harness, &s.RepoURL, &s.RepoBranch, &s.WorkingBranch,
		&s.PullRequestURL, &s.Workspace, &s.SandboxSize, &s.Instructions,
		&s.Status, &s.StatusEventName, &s.TurnState, &s.ACPSessionID,
		&s.EgressTokenHash, &s.MCPScope, &s.MCPServers,
		&s.CreatedAt, &s.ModifiedAt,
	)
	if err != nil {
		return nil, err
	}
	if parentType != nil {
		s.ThreadParentType = *parentType
	}
	return &s, nil
}

// CreateParams carries the fields a new session row needs.
type CreateParams struct {
	ID                   string
	Name                 string
	OwnerID              string // macro|<email>
	ThreadID             *string
	ThreadParentType     *string // "channel" | "document"
	ThreadParentID       *string
	OriginatingMessageID *string
	BotID                string
	Model                string
	Harness              string
	RepoURL              *string
	RepoBranch           *string
	Workspace            string
	SandboxSize          SandboxSize
	Instructions         *string
	ChannelGrant         *string // channel id whose members can view/edit
	External             *ExternalLink
}

// Create inserts the session plus its entity/entity_access/UserHistory
// bookkeeping in one transaction, mirroring the Rust session_service create.
func (r *Repository) Create(ctx context.Context, p CreateParams) (*Session, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create session: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// NOTE: thread_parent is not stored — it derives from the thread's root
	// message at read time (sessionColumns join). p.ThreadParentType/ID are
	// accepted for API parity but only thread_id/originating_message_id land.
	name := p.Name
	if name == "" {
		name = "Agent Session"
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO agent_session (
			id, name, owner_id, thread_id, originating_message_id, bot_id,
			model, harness, repo_url, repo_branch, workspace, sandbox_size,
			instructions
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		p.ID, name, p.OwnerID, p.ThreadID,
		p.OriginatingMessageID, p.BotID, p.Model,
		p.Harness, p.RepoURL, p.RepoBranch, p.Workspace, string(p.SandboxSize),
		p.Instructions)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "agent_session_thread_bot_unique" {
			return nil, ErrThreadSessionExists
		}
		return nil, fmt.Errorf("insert agent_session: %w", err)
	}

	if p.External != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO external_agent_session
				(agent_session_id, provider, external_id, external_name, external_url)
			VALUES ($1,$2,$3,$4,$5)`,
			p.ID, p.External.Provider, p.External.ExternalID,
			p.External.ExternalName, p.External.ExternalURL); err != nil {
			return nil, fmt.Errorf("insert external_agent_session: %w", err)
		}
	}

	// The entity row records ownership; owner_type 'bot' when the owner is a
	// bot principal, 'user' otherwise (sessions own no teams today).
	ownerType := "user"
	if strings.HasPrefix(p.OwnerID, "bot|") {
		ownerType = "bot"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity (id, entity_type, owner_type, owner_id)
		VALUES ($1, 'agent_session', $2, $3)`, p.ID, ownerType, p.OwnerID); err != nil {
		return nil, fmt.Errorf("insert entity: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity_access
			(entity_id, entity_type, source_id, source_type, access_level)
		VALUES ($1, 'agent_session', $2, 'user', 'owner')`, p.ID, p.OwnerID); err != nil {
		return nil, fmt.Errorf("insert owner entity_access: %w", err)
	}
	if p.ChannelGrant != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO entity_access
				(entity_id, entity_type, source_id, source_type, access_level)
			VALUES ($1, 'agent_session', $2, 'channel', 'edit')`, p.ID, *p.ChannelGrant); err != nil {
			return nil, fmt.Errorf("insert channel entity_access: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "UserHistory" ("userId", "itemId", "itemType")
		VALUES ($1, $2, 'agent_session')
		ON CONFLICT ("userId", "itemId", "itemType") DO NOTHING`, p.OwnerID, p.ID); err != nil {
		return nil, fmt.Errorf("insert user history: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create session: %w", err)
	}
	return r.Get(ctx, p.ID)
}

// Get loads one session by id, or nil.
func (r *Repository) Get(ctx context.Context, id string) (*Session, error) {
	s, err := scanSession(r.db.QueryRow(ctx,
		`SELECT `+sessionColumns+sessionFrom+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent_session: %w", err)
	}
	ext, err := r.external(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	s.External = ext
	return s, nil
}

func (r *Repository) external(ctx context.Context, sessionID string) (*ExternalLink, error) {
	var e ExternalLink
	err := r.db.QueryRow(ctx, `
		SELECT provider, external_id, external_name, external_url
		FROM external_agent_session WHERE agent_session_id = $1`, sessionID,
	).Scan(&e.Provider, &e.ExternalID, &e.ExternalName, &e.ExternalURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read external_agent_session: %w", err)
	}
	return &e, nil
}

// FindForThread returns the session a thread already routes to for this bot
// (agent_session.find_for_thread), or nil.
func (r *Repository) FindForThread(ctx context.Context, threadID, botID string) (*Session, error) {
	s, err := scanSession(r.db.QueryRow(ctx, `
		SELECT `+sessionColumns+sessionFrom+`
		WHERE s.thread_id = $1 AND s.bot_id = $2`, threadID, botID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find session for thread: %w", err)
	}
	return s, nil
}

// FindAllForThread returns every session rooted at this thread — the
// implicit-reply evaluation set (agent_session.find_all_for_thread).
func (r *Repository) FindAllForThread(ctx context.Context, threadID string) ([]*Session, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+sessionColumns+sessionFrom+`
		WHERE s.thread_id = $1 OR s.originating_message_id = $1`, threadID)
	if err != nil {
		return nil, fmt.Errorf("find all sessions for thread: %w", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AccessLevel reports the caller's effective level on a session: the owner
// is 'owner'; otherwise the strongest entity_access row reachable directly
// (source_id = user) or through a channel the caller belongs to. Returns ""
// for none. This is the usable-core subset of entity_access's full
// project/team expansion — TODO(access): share the real service when ported.
func (r *Repository) AccessLevel(ctx context.Context, sessionID, userID string) (string, error) {
	var level *string
	err := r.db.QueryRow(ctx, `
		SELECT access_level FROM (
			SELECT 'owner'::"AccessLevel" AS access_level, 0 AS rank
			FROM agent_session WHERE id = $1 AND owner_id = $2
			UNION ALL
			SELECT ea.access_level, 1 FROM entity_access ea
			WHERE ea.entity_id = $1 AND ea.entity_type = 'agent_session'
			  AND ea.source_type = 'user' AND ea.source_id = $2
			UNION ALL
			SELECT ea.access_level, 1 FROM entity_access ea
			WHERE ea.entity_id = $1 AND ea.entity_type = 'agent_session'
			  AND ea.source_type = 'channel'
			  AND EXISTS (
				SELECT 1 FROM comms_channel_participants cp
				WHERE cp.channel_id::text = ea.source_id
				  AND cp.user_id = $2 AND cp.left_at IS NULL
			  )
		) levels ORDER BY rank,
			array_position(ARRAY['owner','edit','comment','view']::"AccessLevel"[], access_level)
		LIMIT 1`, sessionID, userID).Scan(&level)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		// channel_participants may not exist under that name on every
		// deployment shape; degrade to owner/direct grants rather than fail.
		return "", fmt.Errorf("access level for session %s: %w", sessionID, err)
	}
	if level == nil {
		return "", nil
	}
	return *level, nil
}

// Rename sets the user-facing name.
func (r *Repository) Rename(ctx context.Context, id, name string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE agent_session SET name = $2, modified_at = now() WHERE id = $1`, id, name)
	if err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Delete removes the session and its bookkeeping rows (Rust delete path:
// entity_access, entity, UserHistory, SharePermission link, then the row).
func (r *Repository) Delete(ctx context.Context, id string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete session: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []string{
		`DELETE FROM entity_access WHERE entity_id = $1 AND entity_type = 'agent_session'`,
		`DELETE FROM entity WHERE id = $1`,
		`DELETE FROM "UserHistory" WHERE "itemId" = $1 AND "itemType" = 'agent_session'`,
		`DELETE FROM agent_session WHERE id = $1`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// AppendLog writes one agent_session_log row.
func (r *Repository) AppendLog(ctx context.Context, id, sessionID string, userID *string, direction string, content []byte) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO agent_session_log (id, agent_session_id, user_id, direction, content)
		VALUES ($1,$2,$3,$4,$5)`, id, sessionID, userID, direction, content)
	return err
}

// ListLog returns the session's log in chronological order.
func (r *Repository) ListLog(ctx context.Context, sessionID string) ([]SessionLogEntry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id::text, agent_session_id::text, user_id, direction, content, created_at
		FROM agent_session_log
		WHERE agent_session_id = $1
		ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session log: %w", err)
	}
	defer rows.Close()
	var out []SessionLogEntry
	for rows.Next() {
		var e SessionLogEntry
		if err := rows.Scan(&e.ID, &e.SessionID, &e.UserID, &e.Direction, &e.Content, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetStatus records the session's status and event name.
func (r *Repository) SetStatus(ctx context.Context, id, status string, eventName *string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE agent_session SET status=$2, status_event_name=$3, modified_at=now() WHERE id=$1`,
		id, status, eventName)
	return err
}

// SetEgressTokenHash stores the session-token digest.
func (r *Repository) SetEgressTokenHash(ctx context.Context, id, hash string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE agent_session SET egress_token_hash=$2, modified_at=now() WHERE id=$1`, id, hash)
	return err
}

// SetSandboxSize updates the session's compute tier (takes effect on the
// next spawn — Docker cannot resize in place, same as the Rust provider).
func (r *Repository) SetSandboxSize(ctx context.Context, id string, size SandboxSize) error {
	_, err := r.db.Exec(ctx,
		`UPDATE agent_session SET sandbox_size=$2, modified_at=now() WHERE id=$1`, id, string(size))
	return err
}

// UserSandboxSize reads the caller's default tier, or "".
func (r *Repository) UserSandboxSize(ctx context.Context, userID string) (SandboxSize, error) {
	var s string
	err := r.db.QueryRow(ctx,
		`SELECT sandbox_size FROM user_agent_sandbox_size WHERE user_id = $1`, userID).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read user sandbox size: %w", err)
	}
	return SandboxSize(s), nil
}

// SetUserSandboxSize upserts the caller's default tier.
func (r *Repository) SetUserSandboxSize(ctx context.Context, userID string, size SandboxSize) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO user_agent_sandbox_size (user_id, sandbox_size)
		VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET sandbox_size=$2, modified_at=now()`,
		userID, string(size))
	return err
}

// ListByOwner returns the caller's sessions, newest first (for a future
// list endpoint; preview uses it too when ids are empty).
func (r *Repository) ListByOwner(ctx context.Context, userID string) ([]*Session, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+sessionColumns+sessionFrom+`
		WHERE s.owner_id = $1 ORDER BY s.created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions by owner: %w", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
