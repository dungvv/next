package agentharness

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// System bot ids — the compile-time constants of crates/bot_id. System bots
// deliberately have no `bots` row (the table is owned bots only since
// migration 20260826152838), so this registry is their only source of truth.
const (
	// MacroAIBotID is the classic in-channel reply bot (no agent session).
	MacroAIBotID = "00000000-0000-0000-0000-00000000a1a1"
	// MacroNewBotID is the next-gen Macro bot; sessions run on the
	// in-process harness (not this module's managed runtime).
	MacroNewBotID = "00000000-0000-0000-0000-00000000a2a2"
	// MacroSystemBotID is the autonomous platform principal.
	MacroSystemBotID = "00000000-0000-0000-0000-000000005759"
	// MacroCoderBotID is the sandboxed coding-agent bot — the one this
	// module's managed runtime serves by default.
	MacroCoderBotID = "00000000-0000-0000-0000-00000000a9e7"
	// CursorBotID is the externally-served Cursor cloud agent.
	CursorBotID = "00000000-0000-0000-0000-00000000c5c5"
	// CodexBotID is the per-owner Codex cloud agent.
	CodexBotID = "00000000-0000-0000-0000-0000000000c0de"
	// ClaudeBotID is the per-owner Claude cloud agent.
	ClaudeBotID = "00000000-0000-0000-0000-000000000c1a0"
)

// systemBot mirrors bot_id::SystemBot.
type systemBot struct {
	id       string
	name     string
	handle   string
	hasAgent bool
	// provider is set for bots whose sessions an external cloud runtime
	// serves (external_agent_session.provider); empty for managed bots.
	provider string
}

var systemBots = map[string]systemBot{
	MacroAIBotID:     {id: MacroAIBotID, name: "Macro", handle: "macro", hasAgent: false},
	MacroNewBotID:    {id: MacroNewBotID, name: "macro(new)", handle: "macro-new", hasAgent: true},
	MacroCoderBotID:  {id: MacroCoderBotID, name: "Macro Coder", handle: "coder", hasAgent: true},
	CursorBotID:      {id: CursorBotID, name: "Cursor", handle: "cursor", hasAgent: true, provider: "cursor"},
	CodexBotID:       {id: CodexBotID, name: "Codex", handle: "codex", hasAgent: true, provider: "codex"},
	ClaudeBotID:      {id: ClaudeBotID, name: "Claude", handle: "claude", hasAgent: true, provider: "claude"},
	MacroSystemBotID: {id: MacroSystemBotID, name: "Macro System", handle: "macro-system", hasAgent: false},
}

// BotFacts is what session creation and trigger evaluation need to know
// about a bot: whether it is an agent bot, who owns it, and which harness
// serves it (agent_configs row, for user-owned bots).
type BotFacts struct {
	ID       string
	HasAgent bool
	// Owned bots only:
	OwnerUserID *string
	TeamID      *string
	// agent_config fields, when a row exists:
	ConfigHarness   *string
	ConfigHarnessID *string // the user's harness (macrod) serving this bot
	ChannelScope    *string
	// Provider is the external runtime provider for system bots.
	Provider string
}

// botFacts resolves a bot id: system bots come from the compile-time
// registry, owned bots from `bots` left-joined with `agent_configs`.
func botFacts(ctx context.Context, db *pgxpool.Pool, botID string) (*BotFacts, error) {
	if sys, ok := systemBots[botID]; ok {
		return &BotFacts{
			ID:       sys.id,
			HasAgent: sys.hasAgent,
			Provider: sys.provider,
		}, nil
	}
	var f BotFacts
	err := db.QueryRow(ctx, `
		SELECT b.id, b.has_agent, b.owner_user_id, b.team_id,
		       ac.harness, ac.harness_id::text, ac.channel_scope
		FROM bots b
		LEFT JOIN agent_configs ac ON ac.bot_id = b.id
		WHERE b.id = $1 AND b.deleted_at IS NULL`, botID,
	).Scan(&f.ID, &f.HasAgent, &f.OwnerUserID, &f.TeamID,
		&f.ConfigHarness, &f.ConfigHarnessID, &f.ChannelScope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bot facts: %w", err)
	}
	return &f, nil
}
