-- name: UpdateDbThreadMetadata :exec
UPDATE email_threads
        SET
            inbox_visible = $1,
            is_read = $2,
            latest_inbound_message_ts = $3,
            latest_outbound_message_ts = $4,
            latest_non_spam_message_ts = $5,
            updated_at = NOW()
        WHERE
            id = $6 AND
            link_id = $7 AND
            (inbox_visible, is_read, latest_inbound_message_ts, latest_outbound_message_ts, latest_non_spam_message_ts)
                IS DISTINCT FROM ($1, $2, $3, $4, $5);


-- name: UpdateInboxVisibleStatus :exec
UPDATE email_threads
        SET
            inbox_visible = $1,
            updated_at = NOW()
        WHERE
            id = $2 AND
            link_id = $3;


-- name: UpdateThreadReadStatus :exec
UPDATE email_threads
        SET
            is_read = $1,
            updated_at = NOW()
        WHERE
            id = $2 AND
            link_id = $3;


-- name: UpdateThreadProviderId :exec
UPDATE email_threads
        SET
            provider_id = $1,
            updated_at = NOW()
        WHERE
            id = $2 AND
            link_id = $3;


-- name: SyncThreadCalendarFlag2 :exec
UPDATE email_threads t
        SET has_calendar_attachment = calc.has_cal
        FROM (
            SELECT EXISTS (
                SELECT 1
                FROM email_messages m
                JOIN email_attachments a ON a.message_id = m.id
                WHERE m.thread_id = $1
                  AND (a.filename ILIKE '%.ics'
                       OR a.mime_type = 'text/calendar'
                       OR a.mime_type = 'application/ics')
            ) AS has_cal
        ) calc
        WHERE t.id = $1
          AND t.has_calendar_attachment IS DISTINCT FROM calc.has_cal;


-- name: SyncThreadSignalFlag :exec
UPDATE email_threads t
        SET is_signal = calc.sig
        FROM (
            SELECT EXISTS (
                SELECT 1
                FROM email_messages m
                WHERE m.thread_id = $1
                  AND NOT EXISTS (
                      SELECT 1 FROM email_message_labels ml
                      JOIN email_labels l ON ml.label_id = l.id
                      WHERE ml.message_id = m.id AND l.name = 'TRASH'
                  )
                  AND (
                      (
                          EXISTS (
                              SELECT 1
                              FROM email_contacts sender_c
                              JOIN email_filters ef
                                ON ef.link_id = m.link_id
                               AND ef.email_address IS NOT NULL
                               AND LOWER(ef.email_address) = LOWER(sender_c.email_address)
                              WHERE sender_c.id = m.from_contact_id
                                AND ef.is_important = TRUE
                          )
                          OR EXISTS (
                              SELECT 1
                              FROM email_contacts sender_c
                              JOIN email_filters ef
                                ON ef.link_id = m.link_id
                               AND ef.email_domain IS NOT NULL
                               AND LOWER(ef.email_domain) = LOWER(SPLIT_PART(sender_c.email_address, '@', 2))
                              WHERE sender_c.id = m.from_contact_id
                                AND ef.is_important = TRUE
                                AND NOT EXISTS (
                                    SELECT 1 FROM email_filters ef_addr
                                    WHERE ef_addr.link_id = m.link_id
                                      AND ef_addr.email_address IS NOT NULL
                                      AND LOWER(ef_addr.email_address) = LOWER(sender_c.email_address)
                                      AND ef_addr.is_important = FALSE
                                )
                          )
                      )
                      OR (
                          NOT (
                              EXISTS (
                                  SELECT 1
                                  FROM email_contacts sender_c
                                  JOIN email_filters ef
                                    ON ef.link_id = m.link_id
                                   AND ef.email_address IS NOT NULL
                                   AND LOWER(ef.email_address) = LOWER(sender_c.email_address)
                                  WHERE sender_c.id = m.from_contact_id
                                    AND ef.is_important = FALSE
                              )
                              OR EXISTS (
                                  SELECT 1
                                  FROM email_contacts sender_c
                                  JOIN email_filters ef
                                    ON ef.link_id = m.link_id
                                   AND ef.email_domain IS NOT NULL
                                   AND LOWER(ef.email_domain) = LOWER(SPLIT_PART(sender_c.email_address, '@', 2))
                                  WHERE sender_c.id = m.from_contact_id
                                    AND ef.is_important = FALSE
                                    AND NOT EXISTS (
                                        SELECT 1 FROM email_filters ef_addr
                                        WHERE ef_addr.link_id = m.link_id
                                          AND ef_addr.email_address IS NOT NULL
                                          AND LOWER(ef_addr.email_address) = LOWER(sender_c.email_address)
                                          AND ef_addr.is_important = TRUE
                                    )
                              )
                          )
                          AND (
                              m.is_draft = TRUE
                              OR EXISTS (
                                  SELECT 1 FROM email_message_labels ml
                                  JOIN email_labels l ON ml.label_id = l.id
                                  WHERE ml.message_id = m.id
                                    AND l.name IN ('CATEGORY_PERSONAL', 'SENT', 'DRAFT')
                              )
                              OR NOT EXISTS (
                                  SELECT 1 FROM email_message_labels ml
                                  JOIN email_labels l ON ml.label_id = l.id
                                  WHERE ml.message_id = m.id
                                    AND l.name IN ('CATEGORY_UPDATES', 'CATEGORY_PROMOTIONS', 'CATEGORY_SOCIAL', 'CATEGORY_FORUMS')
                              )
                          )
                      )
                  )
            ) AS sig
        ) calc
        WHERE t.id = $1
          AND t.is_signal IS DISTINCT FROM calc.sig;

