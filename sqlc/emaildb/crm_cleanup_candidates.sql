-- name: InsertCandidates :exec
INSERT INTO crm_cleanup_candidates (link_id, contact_email)
        SELECT $1, unnest($2::text[])
        ON CONFLICT (link_id, contact_email) DO UPDATE SET created_at = now();


-- name: ListCandidatesPage :many
SELECT id, link_id, contact_email, created_at
        FROM crm_cleanup_candidates
        WHERE id > $1 AND id <= $2
        ORDER BY id
        LIMIT $3;


-- name: ClaimCandidates :exec
DELETE FROM crm_cleanup_candidates c
        USING (SELECT unnest($1::uuid[]) AS link_id, unnest($2::text[]) AS contact_email) AS p
        WHERE c.link_id = p.link_id AND c.contact_email = p.contact_email;


-- name: ClaimCandidate :exec
DELETE FROM crm_cleanup_candidates
        WHERE link_id = $1 AND contact_email = $2;


-- name: GetMaxIdAndCount :one
SELECT MAX(id)::int8 as "max_id", COUNT(*) as "count"
        FROM crm_cleanup_candidates;

