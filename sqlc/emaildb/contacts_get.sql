-- name: GetSenderByMessageId :one
SELECT
                c.id,
                c.link_id,
                c.email_address,
                COALESCE(m.from_name, c.name) as "name", -- name from message overrides contact name
                c.original_photo_url,
                c.sfs_photo_url,
                c.created_at,
                c.updated_at
            FROM email_messages m
            INNER JOIN email_contacts c ON c.id = m.from_contact_id
            WHERE m.id = $1
            AND m.from_contact_id IS NOT NULL;


-- name: FetchSentMessageRecipientContactsByLink :many
SELECT
            LOWER(c.email_address)   AS "email",
            c.name                   AS "name",
            MIN(m.internal_date_ts)::timestamptz  AS "first_at",
            MAX(m.internal_date_ts)::timestamptz  AS "last_at"
        FROM email_messages m
        JOIN email_message_recipients r ON r.message_id = m.id
        JOIN email_contacts c ON c.id = r.contact_id
        WHERE m.link_id = $1
          AND m.is_sent = true
          AND m.internal_date_ts IS NOT NULL
        GROUP BY LOWER(c.email_address), c.name;


-- name: FetchReceivedSenderContactsByLink :many
SELECT
            LOWER(c.email_address)   AS "email",
            c.name                   AS "name",
            MIN(m.internal_date_ts)::timestamptz  AS "first_at",
            MAX(m.internal_date_ts)::timestamptz  AS "last_at"
        FROM email_messages m
        JOIN email_contacts c ON c.id = m.from_contact_id
        WHERE m.link_id = $1
          AND m.is_sent = false
          AND m.from_contact_id IS NOT NULL
          AND m.internal_date_ts IS NOT NULL
        GROUP BY LOWER(c.email_address), c.name;


-- name: LinkHasAnyMessageWith :one
SELECT EXISTS (
            SELECT 1
            FROM email_messages m
            JOIN email_message_recipients r ON r.message_id = m.id
            JOIN email_contacts c ON c.id = r.contact_id
            WHERE m.link_id = $1
              AND m.is_sent = true
              AND c.link_id = $1
              AND LOWER(c.email_address) = $2
            UNION ALL
            SELECT 1
            FROM email_messages m
            JOIN email_contacts c ON c.id = m.from_contact_id
            WHERE m.link_id = $1
              AND m.is_sent = false
              AND c.link_id = $1
              AND LOWER(c.email_address) = $2
            LIMIT 1
        ) AS "exists";


-- name: FetchDbRecipients :many
SELECT
            mr.message_id,
            c.id,
            c.link_id,
            c.email_address,
            COALESCE(mr.name, c.name) as "name", -- name from message overrides contact name
            c.original_photo_url,
            c.sfs_photo_url,
            c.created_at,
            c.updated_at,
            mr.recipient_type as "recipient_type"
        FROM email_message_recipients mr
        JOIN email_contacts c ON mr.contact_id = c.id
        WHERE mr.message_id = $1
        ORDER BY mr.recipient_type;


-- name: FetchDbRecipientsInBulk :many
SELECT
            mr.message_id,
            c.id,
            c.link_id,
            c.email_address,
            COALESCE(mr.name, c.name) as "name", -- name from message overrides contact name
            c.original_photo_url,
            c.sfs_photo_url,
            c.created_at,
            c.updated_at,
            mr.recipient_type as "recipient_type"
        FROM email_messages m
        JOIN email_message_recipients mr ON m.id = mr.message_id
        JOIN email_contacts c ON mr.contact_id = c.id
        WHERE
            m."id" = ANY($1::uuid[])
        ORDER BY mr.message_id, mr.recipient_type;


-- name: FetchIdByEmail :one
SELECT id
        FROM email_contacts
        WHERE LOWER(email_address) = LOWER($1) AND link_id = $2;


-- name: FetchContactByEmail :one
SELECT id, link_id, email_address, name, original_photo_url, sfs_photo_url, created_at, updated_at
        FROM email_contacts
        WHERE LOWER(email_address) = LOWER($1) AND link_id = $2;


-- name: FetchSenderContactsByMessageIds :many
SELECT
                c.id,
                c.link_id,
                c.email_address,
                COALESCE(m.from_name, c.name) as "name", -- name from message overrides contact name
                c.original_photo_url,
                c.sfs_photo_url,
                c.created_at,
                c.updated_at
            FROM email_messages m
            INNER JOIN email_contacts c ON c.id = m.from_contact_id
            WHERE m."id" = ANY($1::uuid[])
            AND m.from_contact_id IS NOT NULL;


-- name: FetchSendersByMessageIds :many
SELECT
                m.id as message_id,
                c.id,
                c.link_id,
                c.email_address,
                COALESCE(m.from_name, c.name) as "name",
                c.original_photo_url,
                c.sfs_photo_url,
                c.created_at,
                c.updated_at
            FROM email_messages m
            INNER JOIN email_contacts c ON c.id = m.from_contact_id
            WHERE m."id" = ANY($1::uuid[])
            AND m.from_contact_id IS NOT NULL;


-- name: FetchContactsByThreadIds :many
SELECT
            m.thread_id,
            c.email_address as "email_address",
            COALESCE(m.from_name, c.name) as "name"
        FROM email_messages m
        JOIN email_contacts c ON m.from_contact_id = c.id
        WHERE m."thread_id" = ANY($1::uuid[]) AND m.from_contact_id IS NOT NULL
        ORDER BY m.created_at ASC;


-- name: FetchContactsByLinkId :many
WITH
            -- Get all individual interactions with timestamps
            LinkMessageTimestamps AS (
                SELECT
                    m.link_id,
                    mr.contact_id AS contact_address_id,
                    m.internal_date_ts
                FROM
                    email_messages m
                JOIN
                    email_message_recipients mr ON m.id = mr.message_id
                WHERE
                    m.link_id = $1
                    AND m.is_sent = TRUE
                    AND mr.contact_id IS NOT NULL
            ),
            -- Get the latest interaction timestamp for each contact_address_id
            LatestContactInteractions AS (
                SELECT
                    lmt.link_id,
                    lmt.contact_address_id,
                    MAX(lmt.internal_date_ts)::timestamptz AS last_interaction_ts
                FROM
                    LinkMessageTimestamps lmt
                WHERE
                    lmt.contact_address_id IS NOT NULL
                GROUP BY
                    lmt.link_id,
                    lmt.contact_address_id
            )
        -- Final SELECT to join with email_addresses and get details
        SELECT
            c.email_address as "email_address",
            c.name as "name",
            c.sfs_photo_url as "photo_url",
            lci.last_interaction_ts as "last_interaction"
        FROM
            LatestContactInteractions lci
        JOIN
            email_contacts c ON lci.contact_address_id = c.id
        ORDER BY
            lci.last_interaction_ts DESC, c.email_address;


-- name: FetchContactsEmailsByLinkId :many
SELECT DISTINCT
            c.email_address as "email_address"
        FROM
            email_messages m
        JOIN
            email_message_recipients mr ON m.id = mr.message_id
        JOIN
            email_contacts c ON mr.contact_id = c.id
        WHERE
            m.link_id = $1
            AND m.is_sent = TRUE
            AND mr.contact_id IS NOT NULL
        ORDER BY
            c.email_address;

