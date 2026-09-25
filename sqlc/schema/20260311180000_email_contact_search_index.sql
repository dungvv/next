CREATE EXTENSION IF NOT EXISTS btree_gin;

CREATE TABLE email_contact_search_index (
    link_id UUID NOT NULL,
    thread_id UUID NOT NULL,
    message_id UUID NOT NULL,
    contact_name TEXT,
    contact_email TEXT NOT NULL,
    contact_type TEXT NOT NULL,
    CONSTRAINT ecsi_unique UNIQUE (message_id, contact_email, contact_type)
);

CREATE INDEX idx_ecsi_link_name_trgm
    ON email_contact_search_index USING gin (link_id, contact_name gin_trgm_ops);

CREATE INDEX idx_ecsi_link_email_trgm
    ON email_contact_search_index USING gin (link_id, contact_email gin_trgm_ops);

-- Triggers first so any new data between this migration and backfill is captured

-- Trigger: populate FROM contact when a message is inserted/updated




-- Trigger: populate recipient contacts when recipients are inserted




-- Trigger: remove recipient entries when recipients are deleted




-- Trigger: cascade message deletion to index




-- Trigger: propagate contact name updates to index



