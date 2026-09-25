-- Add the share permission to all ephemeral calls
ALTER TABLE calls ADD COLUMN share_permission_id TEXT NOT NULL REFERENCES "SharePermission"(id) ON DELETE CASCADE;

-- Add the share permission to all calls records
ALTER TABLE call_records ADD COLUMN share_permission_id TEXT NOT NULL REFERENCES "SharePermission"(id) ON DELETE CASCADE;

-- Automatically delete the share permission of the call_record when a call record is deleted
-- We only want this for call_records as they are the permanent record of a call not the ephemeral one for active calls



