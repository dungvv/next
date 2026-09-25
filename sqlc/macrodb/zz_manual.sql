-- Manual static ports of dynamic (format!/QueryBuilder) queries from
-- crates/macro_db_client. This file is hand-maintained: do NOT delete when
-- re-running the extractor.

-- create_channel_share_permissions (share_permission/channel_permission/create.rs)
-- Rust builds `VALUES ($1, $n, $n+1), ...` per item; equivalent batch via UNNEST.
-- name: CreateChannelSharePermissions :exec
INSERT INTO "ChannelSharePermission" ("share_permission_id", "channel_id", "access_level")
SELECT $1, u.channel_id, u.access_level::"AccessLevel"
FROM (
    SELECT unnest($2::text[]) AS channel_id,
           unnest($3::text[]) AS access_level
) u
ON CONFLICT ("share_permission_id", "channel_id") DO NOTHING;


-- get_user_documents count (document/get_user_documents.rs)
-- Rust appends `AND d."fileType" = $2` only when file_type is Some.
-- name: GetUserDocumentsCount :one
SELECT COUNT(*) AS "count"
FROM "Document" d
WHERE d.owner = $1 AND d."deletedAt" IS NULL
    AND ($2::text IS NULL OR d."fileType" = $2);


-- get_user_documents main query (document/get_user_documents.rs)
-- Same optional fileType filter; ORDER BY d."updatedAt" DESC is fixed in Rust.
-- name: GetUserDocuments :many
SELECT
    d.id AS document_id,
    d.owner AS owner,
    d.name AS document_name,
    COALESCE(db.id, di.id) AS "document_version_id",
    d."branchedFromId" AS branched_from_id,
    d."branchedFromVersionId" AS branched_from_version_id,
    d."documentFamilyId" AS document_family_id,
    d."fileType" AS file_type,
    d."createdAt"::timestamptz AS created_at,
    d."updatedAt"::timestamptz AS updated_at,
    db.bom_parts AS "document_bom",
    di.modification_data AS "modification_data",
    d."projectId" AS "project_id",
    p.name AS "project_name",
    di.sha AS "sha",
    dt.sub_type AS "sub_type",
    d."deletedAt"::timestamptz AS deleted_at
FROM "Document" d
LEFT JOIN document_sub_type dt ON dt.document_id = d.id
LEFT JOIN LATERAL (
    SELECT
        b.id,
        (
            SELECT json_agg(
                json_build_object(
                    'id', bp.id,
                    'sha', bp.sha,
                    'path', bp.path
                )
            )
            FROM "BomPart" bp
            WHERE bp."documentBomId" = b.id
        ) AS bom_parts
    FROM "DocumentBom" b
    WHERE b."documentId" = d.id
    ORDER BY b."createdAt" DESC
    LIMIT 1
) db ON d."fileType" = 'docx'
LEFT JOIN LATERAL (
    SELECT
        i.id,
        i."documentId",
        i."sha",
        i."createdAt",
        (
            SELECT imod."modificationData"
            FROM "DocumentInstanceModificationData" imod
            WHERE imod."documentInstanceId" = i.id
        ) AS modification_data,
        i."updatedAt"
    FROM "DocumentInstance" i
    WHERE i."documentId" = d.id
    ORDER BY i."updatedAt" DESC
    LIMIT 1
) di ON d."fileType" IS DISTINCT FROM 'docx'
LEFT JOIN LATERAL (
    SELECT p.name
    FROM "Project" p
    WHERE p.id = d."projectId"
) p ON d."projectId" IS NOT NULL
WHERE
    d.owner = $1 AND d."deletedAt" IS NULL
    AND ($4::text IS NULL OR d."fileType" = $4)
ORDER BY d."updatedAt" DESC
LIMIT $2 OFFSET $3;


-- create_onboarding_documents step 1 (document/initialize_onboarding_documents.rs)
-- Rust interpolates `('<user_id>', '<name>', '<file_type>', '<project_id>')` rows.
-- name: CreateOnboardingDocuments :many
INSERT INTO "Document" (owner, name, "fileType", "projectId")
SELECT $1, u.name, u.file_type, $4
FROM (
    SELECT unnest($2::text[]) AS name,
           unnest($3::text[]) AS file_type
) u
RETURNING id;


-- create_onboarding_documents step 2: one DocumentInstance per document, sha='sha'
-- name: CreateOnboardingDocumentInstances :many
INSERT INTO "DocumentInstance" ("documentId", "sha")
SELECT u.document_id, 'sha'
FROM (SELECT unnest($1::text[]) AS document_id) u
RETURNING id;


-- create_onboarding_documents step 3: UserHistory rows (RETURNING unused in Rust)
-- name: CreateOnboardingUserHistory :exec
INSERT INTO "UserHistory" ("userId", "itemId", "itemType")
SELECT $1, u.item_id, 'document'
FROM (SELECT unnest($2::text[]) AS item_id) u;


-- insert_bom_parts (document/save_document.rs)
-- name: InsertBomParts :many
INSERT INTO "BomPart" ("documentBomId", "sha", "path")
SELECT $1, u.sha, u.path
FROM (
    SELECT unnest($2::text[]) AS sha,
           unnest($3::text[]) AS path
) u
RETURNING id, sha, path;


-- save_bom_parts_to_db (docx_unzip.rs) — same shape, separate call site
-- name: SaveBomPartsToDb :many
INSERT INTO "BomPart" ("documentBomId", "sha", "path")
SELECT $1, u.sha, u.path
FROM (
    SELECT unnest($2::text[]) AS sha,
           unnest($3::text[]) AS path
) u
RETURNING id, sha, path;


-- edit_share_permission (share_permission/edit.rs)
-- Rust QueryBuilder applies optional assignments. Static equivalent:
--   $2 link_share mode: 0 = leave unchanged, 1 = set NULL (clears access level),
--                       2 = set to $3 (also sets access level to $4)
--   $5: when true and mode=0, apply "linkShareAccessLevel" = CASE WHEN "linkShare"
--       IS NULL THEN NULL ELSE $4 END (covers Some/None access-level-only edits;
--       caller passes the resolved default for the Some(None) case).
-- name: EditSharePermission :exec
UPDATE "SharePermission" SET
    "updatedAt" = NOW(),
    "linkShare" = CASE
        WHEN $2::int = 1 THEN NULL
        WHEN $2::int = 2 THEN $3::text
        ELSE "linkShare"
    END,
    "linkShareAccessLevel" = CASE
        WHEN $2::int = 1 THEN NULL
        WHEN $2::int = 2 THEN $4::"AccessLevel"
        WHEN $5::bool THEN CASE WHEN "linkShare" IS NULL THEN NULL ELSE $4::"AccessLevel" END
        ELSE "linkShareAccessLevel"
    END
WHERE id = $1;
