CREATE TABLE contacts_backfill_outbox (
    id                SERIAL PRIMARY KEY,
    comms_channel_id  uuid        NOT NULL REFERENCES comms_channels(id),
    user_ids          jsonb       NOT NULL,
    applied_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_contacts_backfill_outbox_applied_at ON contacts_backfill_outbox(applied_at);


