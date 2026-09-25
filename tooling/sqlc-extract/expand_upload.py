#!/usr/bin/env python3
"""Expand the format!-assembled attachment-upload queries in
email_db_client/attachments/provider/upload.rs into static sqlc queries by
inlining the SQL fragments built by macros in upload_filters.rs."""
import re

SRC = '/home/dungvv/projects/next/crates/email_db_client/src/attachments/provider/upload.rs'
OUT = '/home/dungvv/projects/next/sqlc/emaildb/attachments_provider_upload.sql'

src = open(SRC).read()

DOC_MIMES = ['application/pdf',
 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
 'application/msword','text/html','text/plain','pdf']
OCTET_EXT = ['PDF','DOC','DOCX','TXT','HTML']
MEDIA_PREFIXES = ['image/','video/']
DOMAINS = ["docusign.com","hellosign.com","dropboxsign.com","adobesign.com","signnow.com","pandadoc.com",
"quickbooks.com","xero.com","stripe.com","paypal.com","squareup.com","bill.com","gusto.com",
"justworks.com","rippling.com","intuit.com","chase.com","bankofamerica.com","wellsfargo.com",
"capitalone.com","amex.com","citibank.com","robinhood.com","etrade.com","fidelity.com","schwab.com",
"interactivebrokers.com","vanguard.com","plaid.com","irs.gov","ssa.gov","uscis.gov","treasury.gov",
"efiletexas.gov","efilemanager.com","efile.ca.gov","sec.gov","greenhouse.io","lever.co","bamboohr.com",
"workday.com","sap.com","indeed.com","linkedin.com","ziprecruiter.com","docusign.net","dropbox.com",
"box.com","drive.google.com","sharepoint.com","onedrive.live.com","wetransfer.com","figma.com",
"canva.com","notion.so","clickup.com","airtable.com","unitedhealthcare.com","aetna.com","cigna.com",
"metlife.com","anthem.com","oscarhealth.com","delta-dental.com","vanguardbenefits.com",
"fidelitybenefits.com","aws.amazon.com","cloudflare.com","digitalocean.com","github.com","gitlab.com",
"atlassian.com","openai.com","anthropic.com"]

ATTACHMENT_MIME_TYPE_FILTERS = (
    "\n    AND (\n        a.mime_type IN (\n"
    + ''.join(f"            '{m}',\n" for m in DOC_MIMES[:-1])
    + f"            '{DOC_MIMES[-1]}'\n        )\n"
    + "        OR (\n            a.mime_type = 'application/octet-stream' \n"
    + "            AND UPPER(SUBSTRING(a.filename FROM '\\.([^.]+)$')) IN ("
    + ''.join(f"'{e}', " for e in OCTET_EXT[:-1])
    + f"'{OCTET_EXT[-1]}')\n        )\n    )\n"
)
ATTACHMENT_MIME_TYPE_FILTERS_WITH_MEDIA = (
    "\n    (a.mime_type LIKE 'image/%' OR a.mime_type LIKE 'video/%')\n"
)
ATTACHMENT_WHITELISTED_DOMAINS = (
    "\n                        OR (\n"
    "                            -- condition 4: email from whitelisted domain\n"
    "                            c.email_address IS NOT NULL\n"
    "                            AND LOWER(SPLIT_PART(c.email_address, '@', 2)) IN (\n"
    + ''.join(f"                                '{d}',\n" for d in DOMAINS[:-1])
    + f"                                '{DOMAINS[-1]}'\n                            )\n                        )"
)

pat = re.compile(r'format!\(\s*r#"(.*?)"#,\s*([^)]*)\)', re.S)
fragmap = {
    'ATTACHMENT_MIME_TYPE_FILTERS': ATTACHMENT_MIME_TYPE_FILTERS,
    'ATTACHMENT_MIME_TYPE_FILTERS_WITH_MEDIA': ATTACHMENT_MIME_TYPE_FILTERS_WITH_MEDIA,
    'ATTACHMENT_WHITELISTED_DOMAINS': ATTACHMENT_WHITELISTED_DOMAINS,
}

def camel(s): return ''.join(w.capitalize() for w in s.split('_'))

out = []
seen = {}
for m in pat.finditer(src):
    body, args = m.group(1), m.group(2).strip()
    arglist = [a.strip() for a in args.split(',') if a.strip()]
    frags = [fragmap[a] for a in arglist]
    parts = body.split('{}')
    assert len(parts)-1 == len(frags)
    sql = parts[0]
    for frag, part in zip(frags, parts[1:]):
        sql += frag + part
    fname = list(re.finditer(r'fn\s+([a-z_0-9]+)', src[:m.start()]))[-1].group(1)
    name = camel(fname)
    seen[name] = seen.get(name, 0) + 1
    if seen[name] > 1:
        name = f'{name}Condition5'  # second query in new_email_document_atts
    out.append(f'-- name: {name} :many\n{sql.strip()};\n')

existing = open(OUT).read() if __import__('os').path.exists(OUT) else ''
# drop any previously appended block
existing = existing.split('-- name: ThreadDocumentAttsForBackfill')[0].rstrip() + '\n\n'
open(OUT, 'w').write(existing + '\n'.join(out))
print('wrote', len(out), 'queries')
