#!/usr/bin/env python3
"""Post-process extracted sqlc .sql files: strip sqlx annotations, rewrite
constructs sqlc can't handle (multi-arg UNNEST, lateral alias refs, etc.)."""
import re, glob, sys

BASE = '/home/dungvv/projects/next/sqlc'

def fix(path, old, new, count=1):
    s = open(path).read()
    if old not in s:
        print('already applied / missing:', path, old[:50].replace(chr(10), ' '))
        return
    open(path, 'w').write(s.replace(old, new, count))

# ---------- global: strip sqlx "name!"/"name?"/"name: Type" alias annotations ----------
alias_pat = re.compile(r'(?i)(as\s+)"([a-zA-Z0-9_]+)([!?])?(:[^"]*)?"')
for f in glob.glob(f'{BASE}/macrodb/*.sql') + glob.glob(f'{BASE}/emaildb/*.sql'):
    s = open(f).read()
    s2 = alias_pat.sub(lambda m: m.group(1) + '"' + m.group(2) + '"', s)
    if s2 != s:
        open(f, 'w').write(s2)

# ---------- generic: multi-arg UNNEST in FROM/USING -> derived SELECT ----------
# FROM|USING UNNEST($1::t[], $2::u[]) AS alias(c1, c2)
multi_alias = re.compile(
    r'(?i)\b(FROM|USING)\s+UNNEST\(([^)]*)\)\s+AS\s+(\w+)\s*\(([^)]*)\)')

def unnest_alias_sub(m):
    kw, args, alias, cols = m.groups()
    args = [a.strip() for a in args.split(',')]
    cols = [c.strip() for c in cols.split(',')]
    assert len(args) == len(cols), m.group(0)
    inner = ', '.join(f'unnest({a}) AS {c}' for a, c in zip(args, cols))
    return f'{kw} (SELECT {inner}) AS {alias}'

# SELECT * FROM UNNEST(args)   (whitespace-flexible)
star_unnest = re.compile(r'(?i)SELECT\s+\*\s+FROM\s+UNNEST\(([^)]*)\)')

def star_sub(m):
    args = [a.strip() for a in m.group(1).split(',')]
    return 'SELECT ' + ', '.join(f'unnest({a})' for a in args)

for f in glob.glob(f'{BASE}/macrodb/*.sql') + glob.glob(f'{BASE}/emaildb/*.sql'):
    s = open(f).read()
    s2 = multi_alias.sub(unnest_alias_sub, s)
    s2 = star_unnest.sub(star_sub, s2)
    if s2 != s:
        open(f, 'w').write(s2)

# ---------- contacts_upsert_sync: `, NOW()` cross join after star-unnest ----------
s = open(f'{BASE}/emaildb/contacts_upsert_sync.sql').read()
if '), NOW()' in s:
    s = s.replace(
        'SELECT unnest($1::uuid[]), unnest($2::uuid[]), unnest($3::varchar[]), unnest($4::varchar[]), unnest($5::text[]), unnest($6::text[]), NOW()',
        '''SELECT u.c1, u.c2, u.c3, u.c4, u.c5, u.c6, NOW()
    FROM (SELECT unnest($1::uuid[]) AS c1, unnest($2::uuid[]) AS c2,
                 unnest($3::varchar[]) AS c3, unnest($4::varchar[]) AS c4,
                 unnest($5::text[]) AS c5, unnest($6::text[]) AS c6) u''')
    open(f'{BASE}/emaildb/contacts_upsert_sync.sql','w').write(s)

# ---------- annotations_get: drop test-fixture inserts (stray backslash etc.) ----------
s = open(f'{BASE}/macrodb/annotations_get.sql').read()
s = re.sub(r'-- name: PlaceablesOnSharedDiscussionsAreReadWithoutALegacyThread\d* :exec\n.*?(?=-- name:|\Z)', '', s, flags=re.S)
open(f'{BASE}/macrodb/annotations_get.sql','w').write(s)

# ---------- annotations_create_anchor: multi-arg UNNEST multi-line ----------
s = open(f'{BASE}/macrodb/annotations_create_anchor.sql').read()
s = s.replace('''    SELECT *
    FROM UNNEST(
        $1::uuid[], 
        $2::double precision[], 
        $3::double precision[], 
        $4::double precision[], 
        $5::double precision[]
    );''','''    SELECT
        unnest($1::uuid[]),
        unnest($2::double precision[]),
        unnest($3::double precision[]),
        unnest($4::double precision[]),
        unnest($5::double precision[]);''')
open(f'{BASE}/macrodb/annotations_create_anchor.sql','w').write(s)

# ---------- calendar_event: inline LATERAL expr ----------
fix(f'{BASE}/macrodb/calendar_event_get_events_for_search.sql',
    '''            FROM calendar_event_occurrences instance
            CROSS JOIN LATERAL (
                SELECT COALESCE(
                    instance.starts_at,
                    instance.start_date::timestamp AT TIME ZONE 'UTC'
                ) AS at
            ) instance_start
            WHERE instance.event_id = event.id
              AND NOT instance.is_cancelled
            ORDER BY
                (instance_start.at >= $3) DESC,
                CASE WHEN instance_start.at >= $3 THEN instance_start.at END ASC,
                instance_start.at DESC,
                instance.occurrence_key
            LIMIT 1''',
    '''            FROM calendar_event_occurrences instance
            WHERE instance.event_id = event.id
              AND NOT instance.is_cancelled
            ORDER BY
                (COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') >= $3) DESC,
                CASE WHEN COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') >= $3
                    THEN COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') END ASC,
                COALESCE(instance.starts_at, instance.start_date::timestamp AT TIME ZONE 'UTC') DESC,
                instance.occurrence_key
            LIMIT 1''')

# ---------- dcs_copy_messages: qualify chatId ----------
fix(f'{BASE}/macrodb/dcs_copy_messages.sql', 'WHERE "chatId"=$2',
    'WHERE "ChatMessage"."chatId"=$2')

# ---------- document_delete_document: qualify documentId ----------
fix(f'{BASE}/macrodb/document_delete_document.sql',
    '(SELECT COUNT(*) FROM "DocumentInstance" WHERE "documentId" = $1)',
    '(SELECT COUNT(*) FROM "DocumentInstance" WHERE "DocumentInstance"."documentId" = $1)')
fix(f'{BASE}/macrodb/document_delete_document.sql',
    '(SELECT COUNT(*) FROM "DocumentBom" WHERE "documentId" = $1)',
    '(SELECT COUNT(*) FROM "DocumentBom" WHERE "DocumentBom"."documentId" = $1)')

# ---------- experiment_log: fetch_one w/o RETURNING -> :exec ----------
fix(f'{BASE}/macrodb/experiment_log.sql', '-- name: CompleteExperimentLog :one',
    '-- name: CompleteExperimentLog :exec')

# ---------- share_permission_access_level_chat ----------
fix(f'{BASE}/macrodb/share_permission_access_level_chat.sql',
    'ANY(SELECT id::uuid FROM "Chat" WHERE id = ANY($1) AND "deletedAt" IS NULL)',
    'ANY(SELECT "Chat".id::uuid FROM "Chat" WHERE "Chat".id = ANY($1) AND "Chat"."deletedAt" IS NULL)')

# ---------- share_permission_get ----------
fix(f'{BASE}/macrodb/share_permission_get.sql',
    'SELECT share_permission_id FROM calls WHERE id = $1',
    'SELECT share_permission_id FROM calls WHERE calls.id = $1')
fix(f'{BASE}/macrodb/share_permission_get.sql',
    'SELECT share_permission_id FROM call_records WHERE id = $1',
    'SELECT share_permission_id FROM call_records WHERE call_records.id = $1')
for tbl in ["DocumentPermission","ChatPermission","ProjectPermission"]:
    fix(f'{BASE}/macrodb/share_permission_get.sql',
        f'FROM "{tbl}"\n        WHERE "sharePermissionId" = ANY($1)',
        f'FROM "{tbl}"\n        WHERE "{tbl}"."sharePermissionId" = ANY($1)')

# ---------- document_list_documents_with_access ----------
s = open(f'{BASE}/macrodb/document_list_documents_with_access.sql').read()
s = s.replace('UserAccessibleDocuments AS (', 'user_accessible_documents AS (')
s = s.replace('INNER JOIN UserAccessibleDocuments uad', 'INNER JOIN user_accessible_documents uad')
s = s.replace('CASE $3\n', 'CASE $3::text\n')
open(f'{BASE}/macrodb/document_list_documents_with_access.sql','w').write(s)

# ---------- messages_scheduled_get: qualify both CTE/update WHEREs ----------
s = open(f'{BASE}/emaildb/messages_scheduled_get.sql').read()
s = s.replace('''            FROM email_scheduled_messages
            WHERE link_id = $1 AND message_id = $2
        ), updated AS (''','''            FROM email_scheduled_messages
            WHERE email_scheduled_messages.link_id = $1 AND email_scheduled_messages.message_id = $2
        ), updated AS (''')
s = s.replace('''            UPDATE email_scheduled_messages
            SET processing = true, updated_at = NOW()
            WHERE link_id = $1 AND message_id = $2''','''            UPDATE email_scheduled_messages
            SET processing = true, updated_at = NOW()
            WHERE email_scheduled_messages.link_id = $1 AND email_scheduled_messages.message_id = $2''')
open(f'{BASE}/emaildb/messages_scheduled_get.sql','w').write(s)

# ---------- chat_history: legacy cols renamed (Chat.title->name,
# ChatAttachment.attachmentId->entity_id) ----------
s = open(f'{BASE}/macrodb/chat_history.sql').read()
s = s.replace('c.title as chat_title', 'c.name as chat_title')
s = s.replace('ma."attachmentId" as attachment_id', 'ma.entity_id::text as attachment_id')
open(f'{BASE}/macrodb/chat_history.sql','w').write(s)

# ---------- attachments_provider_upload: qualify ambiguous cols ----------
s = open(f'{BASE}/emaildb/attachments_provider_upload.sql').read()
s = s.replace('''            SELECT DISTINCT thread_id
            FROM public.email_messages
            WHERE link_id = $1
                AND has_attachments = true''','''            SELECT DISTINCT thread_id
            FROM public.email_messages
            WHERE email_messages.link_id = $1
                AND has_attachments = true''')
s = s.replace('''            SELECT thread_id
            FROM email_messages
            WHERE link_id = $2
                AND provider_id = $1''','''            SELECT thread_id
            FROM email_messages
            WHERE email_messages.link_id = $2
                AND email_messages.provider_id = $1''')
open(f'{BASE}/emaildb/attachments_provider_upload.sql','w').write(s)

print('fixups applied')
PY_MARKER = True

# ---------- type disambiguation casts ----------
import re as _re

def app(path, pairs):
    p = f'{BASE}/{path}'
    s = open(p).read()
    for old, new in pairs:
        s = s.replace(old, new)
    open(p, 'w').write(s)

# NOT $n bool params
app('macrodb/item_access_get_accessible_items.sql', [('(NOT $2 OR', '(NOT $2::bool OR')])
# text[] for array_agg coalesce
app('macrodb/calendar_event_get_event_for_index.sql', [
    (') AS "attendee_emails",', ')::text[] AS "attendee_emails",'),
    (') AS "source_titles"', ')::text[] AS "source_titles"'),
])
# NULL column typing in UNION queries
NULLTYPES = {
    '"is_persistent"':'bool','"is_completed"':'bool','"sha"':'text',
    '"sub_type"':'document_sub_type_value','"file_type"':'text',
    '"branched_from_id"':'text','"branched_from_version_id"':'text',
    '"document_family_id"':'text','"document_version_id"':'int8',
    '"project_id"':'text','"deleted_at"':'timestamptz','"user_id"':'text',
    '"name"':'text','"updated_at"':'timestamptz','"created_at"':'timestamptz',
}
for p in ['macrodb/history.sql','macrodb/pins.sql','macrodb/recents_deleted.sql']:
    s = open(f'{BASE}/{p}').read()
    for name, ty in NULLTYPES.items():
        s = s.replace(f'NULL as {name}', f'NULL::{ty} as {name}')
        s = s.replace(f'NULL AS {name}', f'NULL::{ty} as {name}')
    s = s.replace('END as "is_completed"','END::bool as "is_completed"')
    open(f'{BASE}/{p}','w').write(s)
app('macrodb/document_preview.sql', [('END as "is_completed"','END::bool as "is_completed"')])
app('macrodb/user_get_all.sql', [
    ('array_agg(DISTINCT rop."permissionId") AS permissions',
     'array_agg(DISTINCT rop."permissionId")::text[] AS permissions')])
app('macrodb/share_permission_get.sql', [
    ("'[]'\n                ) as \"channel_share_permissions\"",
     "'[]'::jsonb\n                )::jsonb as \"channel_share_permissions\"")])
app('macrodb/dcs_get_chat.sql', [
    ("'[]'::json\n            ) AS attachments", "'[]'::jsonb\n            )::jsonb AS attachments")])
app('macrodb/user_get_user_name.sql', [
    ('req.id as "user_profile_id"','req.id::text as "user_profile_id"'),
    ('END as "first_name"','END::text as "first_name"'),
    ('END as "last_name"','END::text as "last_name"')])
app('emaildb/backfill_job_update.sql', [
    ('completed_at IS NULL AS "pending"','(completed_at IS NULL)::bool AS "pending"')])
app('emaildb/settings.sql', [
    ('COALESCE($2, FALSE)','COALESCE($2::bool, FALSE)'),
    ('signature_on_replies_forwards = COALESCE($2, email_settings.signature_on_replies_forwards)',
     'signature_on_replies_forwards = COALESCE($2::bool, email_settings.signature_on_replies_forwards)'),
    ('signature = COALESCE($3, email_settings.signature)',
     'signature = COALESCE($3::text, email_settings.signature)')])
app('emaildb/crm_cleanup_candidates.sql', [('MAX(id) as "max_id"','MAX(id)::int8 as "max_id"')])
app('emaildb/contacts_get.sql', [
    ('MIN(m.internal_date_ts)  AS "first_at"','MIN(m.internal_date_ts)::timestamptz  AS "first_at"'),
    ('MAX(m.internal_date_ts)  AS "last_at"','MAX(m.internal_date_ts)::timestamptz  AS "last_at"'),
    ('MAX(lmt.internal_date_ts) AS last_interaction_ts','MAX(lmt.internal_date_ts)::timestamptz AS last_interaction_ts')])
app('emaildb/links_get.sql', [
    ('(g.calendar_disabled_at IS NOT NULL) AS "calendar_disabled"',
     '(g.calendar_disabled_at IS NOT NULL)::bool AS "calendar_disabled"')])
_refs_old = "jsonb_path_query_first(headers_jsonb, '$[*] ? (@.name like_regex \"references\" flag \"i\").value') #>> '{}' as \"references\""
_refs_new = "(jsonb_path_query_first(headers_jsonb, '$[*] ? (@.name like_regex \"references\" flag \"i\").value') #>> '{}')::text as \"references\""
app('emaildb/messages_get.sql',
    [(f'NULL as "{c}"', f'NULL::text as "{c}"') for c in
     ['body_text','body_html_sanitized','body_macro']] + [(_refs_old, _refs_new)])
print('type casts applied')

# Chat.id is TEXT — fix wrongly inferred uuid[] casts on Chat "id" comparisons
app('macrodb/share_permission_access_level_chat.sql', [
    ('SELECT id::uuid FROM "Chat" WHERE "id" = ANY($1::uuid[])',
     'SELECT id::uuid FROM "Chat" WHERE "id" = ANY($1::text[])')])
