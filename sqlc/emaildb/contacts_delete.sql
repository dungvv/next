-- name: DeleteMessageRecipients :exec
DELETE FROM email_message_recipients WHERE message_id = $1;

