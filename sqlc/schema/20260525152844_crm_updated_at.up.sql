ALTER TABLE crm_companies
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE crm_contacts
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();



DROP TRIGGER IF EXISTS crm_companies_set_updated_at ON crm_companies;


DROP TRIGGER IF EXISTS crm_contacts_set_updated_at ON crm_contacts;

