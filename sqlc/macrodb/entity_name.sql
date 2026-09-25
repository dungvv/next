-- name: GetEntityNameAndOwner :one
SELECT
                    c.name,
                    c."userId" as user_id
                FROM
                    "Chat" c
                WHERE
                    c.id = $1;


-- name: GetEntityNameAndOwner2 :one
SELECT
                    d.name,
                    d.owner
                FROM
                    "Document" d
                WHERE
                    d.id = $1;


-- name: GetEntityNameAndOwner3 :one
SELECT
                    e.subject,
                    l.macro_id
                FROM
                    "email_messages" e
                JOIN email_links l ON l.id = e.link_id
                WHERE
                    e.thread_id = $1
                LIMIT 1;


-- name: GetEntityNameAndOwner4 :one
SELECT
                    c.name as "name",
                    c.owner_id
                FROM
                    "comms_channels" c
                WHERE
                    c.id = $1;

