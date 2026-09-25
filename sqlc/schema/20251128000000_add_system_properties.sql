-- System Properties Migration - Schema Changes

ALTER TYPE property_entity_type ADD VALUE IF NOT EXISTS 'COMPANY';
ALTER TYPE property_entity_type ADD VALUE IF NOT EXISTS 'TASK';

ALTER TABLE property_definitions ADD COLUMN is_system BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE property_definitions DROP CONSTRAINT owned_by_org_or_user;
ALTER TABLE property_definitions ADD CONSTRAINT owned_by_org_or_user_or_system 
    CHECK (
        is_system = TRUE
        OR organization_id IS NOT NULL
        OR user_id IS NOT NULL
    );

-- Prevent custom properties from having the same display_name as system properties



