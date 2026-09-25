-- Notify notification-service when deleting notifications removes user_notification rows.
--
-- The existing FK from user_notification.notification_id to notification.id uses
-- ON DELETE CASCADE. This BEFORE DELETE trigger runs while those rows are still
-- visible, so listeners can learn which users need realtime notification-delete
-- updates before the cascade removes the rows.



DROP TRIGGER IF EXISTS trg_notify_user_notification_deletes ON notification;


