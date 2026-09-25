-- Team CRM configuration moves from the legacy `__macro:crm-config` property
-- definition hack into real columns on team_crm_settings.
-- Team saved views intentionally stay an opaque jsonb blob for fast iteration.

ALTER TABLE team_crm_settings
    ADD COLUMN edit_stages_role       team_role NOT NULL DEFAULT 'admin',
    ADD COLUMN move_closed_deals_role team_role NOT NULL DEFAULT 'admin',
    ADD COLUMN delete_records_role    team_role NOT NULL DEFAULT 'admin',
    ADD COLUMN closed_stage_ids       uuid[],
    ADD COLUMN team_views             jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN default_team_view_id   text;

COMMENT ON COLUMN team_crm_settings.closed_stage_ids IS
    'Stage option ids counting as closed deals; NULL = label heuristic on the client';
COMMENT ON COLUMN team_crm_settings.team_views IS
    'Opaque array of team saved views, owned by the frontend';

-- Backfill from the legacy `__macro:crm-config` property definitions, whose
-- single select option holds the JSON config in string_value. Invalid or
-- unparsable configs are skipped (columns keep their defaults).

