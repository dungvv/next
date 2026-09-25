-- name: FetchContactsByEmails :many
SELECT id, email_address
        FROM email_contacts
        WHERE link_id = $1 AND "email_address" = ANY($2::varchar(320)[]);


-- name: UpdateMissingContactNames :exec
UPDATE email_contacts
        SET name = data.name, updated_at = now()
        FROM (SELECT unnest($1::uuid[]) as id, unnest($2::text[]) as name) as data
        WHERE email_contacts.id = data.id
          AND email_contacts.name IS NULL;


-- name: InsertNewContacts :many
INSERT INTO email_contacts (id, link_id, email_address, name)
        SELECT unnest($1::uuid[]), unnest($2::uuid[]), unnest($3::text[]), unnest($4::text[])
        ON CONFLICT (link_id, email_address) DO NOTHING
        RETURNING id, email_address;


-- name: UpsertMessageRecipients :exec
DELETE FROM email_message_recipients
        WHERE message_id = $1
          AND (contact_id, recipient_type) NOT IN (
              SELECT contact_id, recipient_type
              FROM (SELECT unnest($2::uuid[]) AS contact_id, unnest($3::email_recipient_type[]) AS recipient_type) AS t
          );


-- name: UpsertMessageRecipients2 :exec
INSERT INTO email_message_recipients (message_id, contact_id, name, recipient_type)
        SELECT unnest($1::uuid[]), unnest($2::uuid[]), unnest($3::text[]), unnest($4::email_recipient_type[])
        ON CONFLICT (message_id, contact_id, recipient_type) DO NOTHING;

