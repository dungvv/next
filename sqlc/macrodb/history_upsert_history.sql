-- name: UpsertItemLastAccessedTimestamp :exec
INSERT INTO "ItemLastAccessed" ("item_id", "item_type", "last_accessed")
        VALUES ($1, $2, $3)
        ON CONFLICT ("item_id", "item_type") DO UPDATE
        SET "last_accessed" = $3;


-- name: UpsertUserHistoryTimestamp :exec
INSERT INTO "UserHistory" ("userId", "itemId", "itemType", "createdAt", "updatedAt")
        VALUES ($1, $2, $3, $4, $4)
        ON CONFLICT ("userId", "itemId", "itemType") DO UPDATE
        SET "updatedAt" = $4;


-- name: InsertUserHistoryBatch :exec
INSERT INTO "UserHistory" ("userId", "itemId", "itemType", "createdAt", "updatedAt")
        SELECT unnest($1::text[]), unnest($2::text[]), unnest($3::text[]), unnest($4::timestamptz[]), unnest($5::timestamptz[])
        ON CONFLICT ("userId", "itemId", "itemType") DO UPDATE
        SET "updatedAt" = EXCLUDED."updatedAt";

