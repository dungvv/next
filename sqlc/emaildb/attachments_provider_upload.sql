-- name: MessageHasUnclaimedDocumentAttachment :one
SELECT EXISTS (
            SELECT 1
            FROM email_attachments a
            JOIN email_messages m ON a.message_id = m.id
            LEFT JOIN document_email de ON de.email_attachment_id = a.id
            WHERE m.link_id = $2
                AND m.provider_id = $1
                AND de.email_attachment_id IS NULL
                AND a.upload_claimed_at IS NULL
                AND a.filename IS NOT NULL
                AND (
                    a."mime_type" = ANY($3::character[])
                    OR (
                        a.mime_type = 'application/octet-stream'
                        AND UPPER(SUBSTRING(a.filename FROM '\.([^.]+)$')) = ANY($4::text[])
                    )
                )
        ) AS "has_candidate";


-- name: FetchAttachmentUploadMetadataById :one
SELECT
            a.id AS attachment_db_id,
            m.provider_id as "email_provider_id",
            a.provider_attachment_id as "provider_attachment_id",
            a.filename as "filename",
            a.mime_type as "mime_type",
            m.internal_date_ts as "internal_date_ts",
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        JOIN email_contacts from_contact ON m.from_contact_id = from_contact.id
        JOIN email_threads t ON m.thread_id = t.id
        WHERE a.id = $1;

-- name: ThreadDocumentAttsForBackfill :many
WITH
        thread_info AS MATERIALIZED (
            SELECT
                t.id AS thread_id,
                t.link_id,
                LOWER(SPLIT_PART(link.email_address, '@', 2)) AS user_domain
            FROM email_threads t
            JOIN email_links link ON link.id = t.link_id
            WHERE t.id = $1
        ),
        important_label AS MATERIALIZED (
            SELECT l.id
            FROM email_labels l
            JOIN thread_info ti ON l.link_id = ti.link_id
            WHERE l.name = 'IMPORTANT'
        )
        SELECT
            a.id AS attachment_db_id,
            m.provider_id as email_provider_id,
            a.provider_attachment_id as provider_attachment_id,
            a.filename as filename,
            a.mime_type as mime_type,
            m.internal_date_ts as internal_date_ts,
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        JOIN email_contacts from_contact ON m.from_contact_id = from_contact.id
        JOIN thread_info ti ON m.thread_id = ti.thread_id
        WHERE m.thread_id = $1
            AND a.filename IS NOT NULL
            -- attachment mime type filters injected below
            
    AND (
        a.mime_type IN (
            'application/pdf',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'application/msword',
            'text/html',
            'text/plain',
            'pdf'
        )
        OR (
            a.mime_type = 'application/octet-stream' 
            AND UPPER(SUBSTRING(a.filename FROM '\.([^.]+)$')) IN ('PDF', 'DOC', 'DOCX', 'TXT', 'HTML')
        )
    )

            AND (
                -- condition 1: the thread contains a sent message
                EXISTS (
                    SELECT 1
                    FROM email_messages sent_message
                    WHERE sent_message.thread_id = ti.thread_id
                        AND sent_message.is_sent = true
                )
                -- condition 2: the thread contains a message with the link's IMPORTANT label
                OR EXISTS (
                    SELECT 1
                    FROM important_label il
                    JOIN email_message_labels ml ON ml.label_id = il.id
                    JOIN email_messages labeled_message ON labeled_message.id = ml.message_id
                    WHERE labeled_message.thread_id = ti.thread_id
                )
                -- conditions 3 and 4 share one sender pass
                OR EXISTS (
                    SELECT 1
                    FROM email_messages sender_message
                    JOIN email_contacts c ON c.id = sender_message.from_contact_id
                    WHERE sender_message.thread_id = ti.thread_id
                        AND (
                            LOWER(SPLIT_PART(c.email_address, '@', 2)) = ti.user_domain
                            -- whitelisted domain check injected below
                            
                        OR (
                            -- condition 4: email from whitelisted domain
                            c.email_address IS NOT NULL
                            AND LOWER(SPLIT_PART(c.email_address, '@', 2)) IN (
                                'docusign.com',
                                'hellosign.com',
                                'dropboxsign.com',
                                'adobesign.com',
                                'signnow.com',
                                'pandadoc.com',
                                'quickbooks.com',
                                'xero.com',
                                'stripe.com',
                                'paypal.com',
                                'squareup.com',
                                'bill.com',
                                'gusto.com',
                                'justworks.com',
                                'rippling.com',
                                'intuit.com',
                                'chase.com',
                                'bankofamerica.com',
                                'wellsfargo.com',
                                'capitalone.com',
                                'amex.com',
                                'citibank.com',
                                'robinhood.com',
                                'etrade.com',
                                'fidelity.com',
                                'schwab.com',
                                'interactivebrokers.com',
                                'vanguard.com',
                                'plaid.com',
                                'irs.gov',
                                'ssa.gov',
                                'uscis.gov',
                                'treasury.gov',
                                'efiletexas.gov',
                                'efilemanager.com',
                                'efile.ca.gov',
                                'sec.gov',
                                'greenhouse.io',
                                'lever.co',
                                'bamboohr.com',
                                'workday.com',
                                'sap.com',
                                'indeed.com',
                                'linkedin.com',
                                'ziprecruiter.com',
                                'docusign.net',
                                'dropbox.com',
                                'box.com',
                                'drive.google.com',
                                'sharepoint.com',
                                'onedrive.live.com',
                                'wetransfer.com',
                                'figma.com',
                                'canva.com',
                                'notion.so',
                                'clickup.com',
                                'airtable.com',
                                'unitedhealthcare.com',
                                'aetna.com',
                                'cigna.com',
                                'metlife.com',
                                'anthem.com',
                                'oscarhealth.com',
                                'delta-dental.com',
                                'vanguardbenefits.com',
                                'fidelitybenefits.com',
                                'aws.amazon.com',
                                'cloudflare.com',
                                'digitalocean.com',
                                'github.com',
                                'gitlab.com',
                                'atlassian.com',
                                'openai.com',
                                'anthropic.com'
                            )
                        )
                        )
                )
            )
        ORDER BY a.id;

-- name: ThreadMediaAttsForBackfill :many
SELECT
            a.id AS attachment_db_id,
            m.provider_id as email_provider_id,
            a.provider_attachment_id as provider_attachment_id,
            a.filename as filename,
            a.mime_type as mime_type,
            m.internal_date_ts as internal_date_ts,
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        JOIN email_contacts from_contact ON m.from_contact_id = from_contact.id
        WHERE m.thread_id = $1
            -- attachment mime type filters injected below
            AND 
    (a.mime_type LIKE 'image/%' OR a.mime_type LIKE 'video/%')

        ORDER BY a.id;

-- name: FetchJobAttachmentsForBackfill :many
WITH
        eligible_threads AS (
            SELECT DISTINCT thread_id
            FROM public.email_messages
            WHERE email_messages.link_id = $1
                AND has_attachments = true
        ),
        self_contact AS (
            SELECT c.id
            FROM public.email_links l
            JOIN public.email_contacts c
                ON c.link_id = l.id
                AND LOWER(c.email_address) = LOWER(l.email_address)
            WHERE l.id = $1
        ),
        contacted AS (
            SELECT DISTINCT emr.contact_id
            FROM public.email_messages sent_message
            JOIN public.email_message_recipients emr ON emr.message_id = sent_message.id
            WHERE sent_message.link_id = $1
                AND sent_message.is_sent = true
        ),
        participants AS (
            SELECT message.thread_id, message.from_contact_id AS contact_id
            FROM eligible_threads eligible
            JOIN public.email_messages message ON message.thread_id = eligible.thread_id
            WHERE message.from_contact_id IS NOT NULL

            -- UNION ALL, not UNION: `qualified_threads` already applies
            -- SELECT DISTINCT to this result, so a set-union dedupe here
            -- sorts the whole participant set only to have the work thrown
            -- away. Duplicates cannot change the final thread set.
            UNION ALL

            SELECT message.thread_id, recipient.contact_id
            FROM eligible_threads eligible
            JOIN public.email_messages message ON message.thread_id = eligible.thread_id
            JOIN public.email_message_recipients recipient ON recipient.message_id = message.id
        ),
        qualified_threads AS (
            SELECT DISTINCT participant.thread_id
            FROM participants participant
            JOIN contacted ON contacted.contact_id = participant.contact_id
            WHERE NOT EXISTS (
                SELECT 1
                FROM self_contact
                WHERE self_contact.id = participant.contact_id
            )
        )

        SELECT
            a.id AS attachment_db_id,
            m.provider_id as email_provider_id,
            a.provider_attachment_id as provider_attachment_id,
            a.filename as filename,
            a.mime_type as mime_type,
            m.internal_date_ts as internal_date_ts,
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM public.email_attachments a
        JOIN public.email_messages m ON a.message_id = m.id
        JOIN public.email_contacts from_contact ON m.from_contact_id = from_contact.id
        WHERE m.thread_id IN (SELECT thread_id FROM qualified_threads)
            -- attachment mime type filters injected below
            
    AND (
        a.mime_type IN (
            'application/pdf',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'application/msword',
            'text/html',
            'text/plain',
            'pdf'
        )
        OR (
            a.mime_type = 'application/octet-stream' 
            AND UPPER(SUBSTRING(a.filename FROM '\.([^.]+)$')) IN ('PDF', 'DOC', 'DOCX', 'TXT', 'HTML')
        )
    )

            AND a.filename IS NOT NULL
        ORDER BY m.internal_date_ts DESC;

-- name: NewEmailDocumentAtts :many
WITH
        thread_info AS MATERIALIZED (
            SELECT
                t.id AS thread_id,
                t.link_id,
                LOWER(SPLIT_PART(link.email_address, '@', 2)) AS user_domain
            FROM email_messages target_message
            JOIN email_threads t ON t.id = target_message.thread_id
            JOIN email_links link ON link.id = t.link_id
            WHERE target_message.link_id = $2
                AND target_message.provider_id = $1
        ),
        important_label AS MATERIALIZED (
            SELECT l.id
            FROM email_labels l
            JOIN thread_info ti ON l.link_id = ti.link_id
            WHERE l.name = 'IMPORTANT'
        ),
        claimed AS (
            UPDATE email_attachments
            SET upload_claimed_at = NOW()
            WHERE id IN (
                SELECT a.id
                FROM email_attachments a
                JOIN email_messages m ON a.message_id = m.id
                JOIN thread_info ti ON m.thread_id = ti.thread_id
                LEFT JOIN document_email de ON de.email_attachment_id = a.id
                WHERE m.link_id = $2
                    AND m.provider_id = $1
                    AND a.filename IS NOT NULL
                    
    AND (
        a.mime_type IN (
            'application/pdf',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'application/msword',
            'text/html',
            'text/plain',
            'pdf'
        )
        OR (
            a.mime_type = 'application/octet-stream' 
            AND UPPER(SUBSTRING(a.filename FROM '\.([^.]+)$')) IN ('PDF', 'DOC', 'DOCX', 'TXT', 'HTML')
        )
    )

                    AND de.email_attachment_id IS NULL
                    AND a.upload_claimed_at IS NULL
                    AND (
                        -- condition 1: the thread contains a sent message
                        EXISTS (
                            SELECT 1
                            FROM email_messages sent_message
                            WHERE sent_message.thread_id = ti.thread_id
                                AND sent_message.is_sent = true
                        )
                        -- condition 2: the thread contains a message with the link's IMPORTANT label
                        OR EXISTS (
                            SELECT 1
                            FROM important_label il
                            JOIN email_message_labels ml ON ml.label_id = il.id
                            JOIN email_messages labeled_message ON labeled_message.id = ml.message_id
                            WHERE labeled_message.thread_id = ti.thread_id
                        )
                        -- conditions 3 and 4 share one sender pass
                        OR EXISTS (
                            SELECT 1
                            FROM email_messages sender_message
                            JOIN email_contacts c ON c.id = sender_message.from_contact_id
                            WHERE sender_message.thread_id = ti.thread_id
                                AND (
                                    LOWER(SPLIT_PART(c.email_address, '@', 2)) = ti.user_domain
                                    
                        OR (
                            -- condition 4: email from whitelisted domain
                            c.email_address IS NOT NULL
                            AND LOWER(SPLIT_PART(c.email_address, '@', 2)) IN (
                                'docusign.com',
                                'hellosign.com',
                                'dropboxsign.com',
                                'adobesign.com',
                                'signnow.com',
                                'pandadoc.com',
                                'quickbooks.com',
                                'xero.com',
                                'stripe.com',
                                'paypal.com',
                                'squareup.com',
                                'bill.com',
                                'gusto.com',
                                'justworks.com',
                                'rippling.com',
                                'intuit.com',
                                'chase.com',
                                'bankofamerica.com',
                                'wellsfargo.com',
                                'capitalone.com',
                                'amex.com',
                                'citibank.com',
                                'robinhood.com',
                                'etrade.com',
                                'fidelity.com',
                                'schwab.com',
                                'interactivebrokers.com',
                                'vanguard.com',
                                'plaid.com',
                                'irs.gov',
                                'ssa.gov',
                                'uscis.gov',
                                'treasury.gov',
                                'efiletexas.gov',
                                'efilemanager.com',
                                'efile.ca.gov',
                                'sec.gov',
                                'greenhouse.io',
                                'lever.co',
                                'bamboohr.com',
                                'workday.com',
                                'sap.com',
                                'indeed.com',
                                'linkedin.com',
                                'ziprecruiter.com',
                                'docusign.net',
                                'dropbox.com',
                                'box.com',
                                'drive.google.com',
                                'sharepoint.com',
                                'onedrive.live.com',
                                'wetransfer.com',
                                'figma.com',
                                'canva.com',
                                'notion.so',
                                'clickup.com',
                                'airtable.com',
                                'unitedhealthcare.com',
                                'aetna.com',
                                'cigna.com',
                                'metlife.com',
                                'anthem.com',
                                'oscarhealth.com',
                                'delta-dental.com',
                                'vanguardbenefits.com',
                                'fidelitybenefits.com',
                                'aws.amazon.com',
                                'cloudflare.com',
                                'digitalocean.com',
                                'github.com',
                                'gitlab.com',
                                'atlassian.com',
                                'openai.com',
                                'anthropic.com'
                            )
                        )
                                )
                        )
                    )
            )
            RETURNING id
        )
        SELECT
            a.id AS attachment_db_id,
            m.provider_id as email_provider_id,
            a.provider_attachment_id as provider_attachment_id,
            a.filename as filename,
            a.mime_type as mime_type,
            m.internal_date_ts as internal_date_ts,
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        JOIN email_contacts from_contact ON m.from_contact_id = from_contact.id
        WHERE a.id IN (SELECT id FROM claimed)
        ORDER BY a.id;

-- name: NewEmailDocumentAttsCondition5 :many
WITH
        message_info AS (
            SELECT thread_id
            FROM email_messages
            WHERE email_messages.link_id = $2
                AND email_messages.provider_id = $1
        ),
        self_contact AS (
            SELECT c.id
            FROM email_links l
            JOIN email_contacts c
                ON c.link_id = l.id
                AND LOWER(c.email_address) = LOWER(l.email_address)
            WHERE l.id = $2
        ),
        participants AS (
            SELECT em.from_contact_id AS contact_id
            FROM email_messages em
            WHERE em.thread_id = (SELECT thread_id FROM message_info)
                AND em.from_contact_id IS NOT NULL
            UNION
            SELECT emr.contact_id
            FROM email_messages em
            JOIN email_message_recipients emr ON emr.message_id = em.id
            WHERE em.thread_id = (SELECT thread_id FROM message_info)
        ),
        claimed AS (
            UPDATE email_attachments
            SET upload_claimed_at = NOW()
            WHERE id IN (
                SELECT a.id
                FROM email_attachments a
                JOIN email_messages m ON a.message_id = m.id
                LEFT JOIN document_email de ON de.email_attachment_id = a.id
                WHERE m.link_id = $2
                    AND m.provider_id = $1
                    AND de.email_attachment_id IS NULL
                    AND a.upload_claimed_at IS NULL
                    AND a.filename IS NOT NULL
                    
    AND (
        a.mime_type IN (
            'application/pdf',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'application/msword',
            'text/html',
            'text/plain',
            'pdf'
        )
        OR (
            a.mime_type = 'application/octet-stream' 
            AND UPPER(SUBSTRING(a.filename FROM '\.([^.]+)$')) IN ('PDF', 'DOC', 'DOCX', 'TXT', 'HTML')
        )
    )

                    AND EXISTS (
                        SELECT 1
                        FROM participants p
                        WHERE NOT EXISTS (
                                SELECT 1
                                FROM self_contact
                                WHERE self_contact.id = p.contact_id
                            )
                            AND EXISTS (
                                SELECT 1
                                FROM email_message_recipients emr
                                JOIN email_messages sent_message ON sent_message.id = emr.message_id
                                WHERE emr.contact_id = p.contact_id
                                    AND sent_message.link_id = $2
                                    AND sent_message.is_sent = true
                            )
                    )
            )
            RETURNING id
        )
        SELECT
            a.id AS attachment_db_id,
            m.provider_id as email_provider_id,
            a.provider_attachment_id as provider_attachment_id,
            a.filename as filename,
            a.mime_type as mime_type,
            m.internal_date_ts as internal_date_ts,
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        JOIN email_contacts from_contact ON m.from_contact_id = from_contact.id
        WHERE a.id IN (SELECT id FROM claimed)
        ORDER BY a.id;

-- name: NewEmailMediaAtts :many
WITH claimed AS (
            UPDATE email_attachments
            SET upload_claimed_at = NOW()
            WHERE id IN (
                SELECT a.id
                FROM email_attachments a
                JOIN email_messages m ON a.message_id = m.id
                LEFT JOIN email_attachments_sfs eas ON eas.attachment_id = a.id
                WHERE m.link_id = $2
                    AND m.provider_id = $1
                    AND 
    (a.mime_type LIKE 'image/%' OR a.mime_type LIKE 'video/%')

                    AND eas.attachment_id IS NULL
                    AND a.upload_claimed_at IS NULL
            )
            RETURNING id
        )
        SELECT
            a.id AS attachment_db_id,
            m.provider_id as email_provider_id,
            a.provider_attachment_id as provider_attachment_id,
            a.filename as filename,
            a.mime_type as mime_type,
            m.internal_date_ts as internal_date_ts,
            m.id as message_db_id,
            m.thread_id as thread_db_id,
            from_contact.email_address as sender_email,
            m.subject as subject
        FROM email_attachments a
        JOIN email_messages m ON a.message_id = m.id
        JOIN email_contacts from_contact ON m.from_contact_id = from_contact.id
        WHERE a.id IN (SELECT id FROM claimed)
        ORDER BY a.id;
