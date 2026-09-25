-- name: GetBasicCloudStorageItemMetadata :one
SELECT
                    d.id as "document_id",
                    d.owner,
                    d.name as "document_name",
                    d."fileType" as "file_type",
                    dst.sub_type as "sub_type"
                FROM
                    "Document" d
                LEFT JOIN document_sub_type dst ON dst.document_id = d.id
                WHERE d.id = $1
                LIMIT 1;


-- name: GetBasicCloudStorageDocumentsMetadata :many
SELECT
            d.id as "document_id",
            d.owner,
            d.name as "document_name",
            d."fileType" as "file_type",
            dst.sub_type as "sub_type"
        FROM
            "Document" d
        LEFT JOIN document_sub_type dst ON dst.document_id = d.id
        WHERE d."id" = ANY($1::text[]);

