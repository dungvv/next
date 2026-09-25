-- When a channel message is soft-deleted, remove any notification rows
-- whose metadata->>'messageId' points at it. user_notification rows cascade
-- via existing FK (migration 20260126170641).





-- Backfill: sweep notifications for already-soft-deleted messages.
-- Predicates mirror the trigger: event_item_type, event_item_id, messageId.
DELETE FROM notification n
WHERE n.event_item_type = 'channel'
  AND n.metadata->>'messageId' IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM comms_messages m
      WHERE m.id::text = n.metadata->>'messageId'
        AND m.channel_id::text = n.event_item_id
        AND m.deleted_at IS NOT NULL
  );
