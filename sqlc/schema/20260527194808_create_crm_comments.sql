-- CRM comment threads. A thread hangs off exactly one CRM entity (a company
-- or a contact) and carries one or more comments. Mirrors the document
-- "Thread"/"Comment" shape closely enough that the frontend reuses the same
-- assembly/rendering logic, but with uuid PKs and team-scoped CRM parents.

CREATE TABLE IF NOT EXISTS crm_thread (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Exactly one of company_id / contact_id is set (enforced below). Both
    -- cascade-delete so a thread never outlives its CRM parent (companies and
    -- contacts are hard-deleted on email-sync disable / hide / depopulate).
    company_id  uuid        REFERENCES crm_companies(id) ON DELETE CASCADE,
    contact_id  uuid        REFERENCES crm_contacts(id)  ON DELETE CASCADE,
    owner       text        NOT NULL REFERENCES "User"(id) ON UPDATE CASCADE ON DELETE CASCADE,
    resolved    boolean     NOT NULL DEFAULT false,
    metadata    jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,

    CONSTRAINT crm_thread_one_parent CHECK (num_nonnulls(company_id, contact_id) = 1)
);

CREATE TABLE IF NOT EXISTS crm_comment (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id   uuid        NOT NULL REFERENCES crm_thread(id) ON DELETE CASCADE,
    owner       text        NOT NULL REFERENCES "User"(id) ON UPDATE CASCADE ON DELETE CASCADE,
    sender      text,
    text        text        NOT NULL,
    "order"     integer,
    metadata    jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);

-- Look up a CRM entity's threads (the GET path).
CREATE INDEX IF NOT EXISTS crm_thread_company_id_idx ON crm_thread (company_id) WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS crm_thread_contact_id_idx ON crm_thread (contact_id) WHERE contact_id IS NOT NULL;
-- Assemble a thread's comments in order (the nesting step).
CREATE INDEX IF NOT EXISTS crm_comment_thread_id_created_at_idx ON crm_comment (thread_id, created_at);

-- Reuse the shared CRM updated_at trigger (same one crm_companies uses).
-- 

DROP TRIGGER IF EXISTS crm_comment_set_updated_at ON crm_comment;

