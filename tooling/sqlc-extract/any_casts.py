#!/usr/bin/env python3
"""Annotate `x = ANY($n)` params with array casts so sqlc emits slice params.
Builds a column->type map from the migration schema; when a column name is
ambiguous across tables, resolves via the query's FROM/JOIN aliases."""
import re, glob, sys

SCHEMA_DIR = '/tmp/macroschema'
BASE = '/home/dungvv/projects/next/sqlc'

# ---------- build table -> {col: type} ----------
tables = {}
def norm(t):
    return t.strip('"').lower()

# crude CREATE TABLE parser
create_re = re.compile(r'CREATE TABLE\s+(?:IF NOT EXISTS\s+)?(?:public\.)?"?([A-Za-z_][\w]*)"?\s*\((.*?)\)\s*;', re.S | re.I)
for f in glob.glob(f'{SCHEMA_DIR}/*.sql'):
    s = open(f).read()
    for m in create_re.finditer(s):
        tname = norm(m.group(1))
        body = m.group(2)
        cols = tables.setdefault(tname, {})
        # split top-level commas
        depth = 0; cur = ''
        parts = []
        for ch in body:
            if ch == '(':
                depth += 1
            elif ch == ')':
                depth -= 1
            if ch == ',' and depth == 0:
                parts.append(cur); cur = ''
            else:
                cur += ch
        parts.append(cur)
        for p in parts:
            p = p.strip()
            cm = re.match(r'"?([A-Za-z_][\w]*)"?\s+([A-Za-z_][\w".()\[\]]*(?:\s+PRECISION)?(?:\[\])?)', p)
            if cm and cm.group(1).upper() not in ('CONSTRAINT','PRIMARY','FOREIGN','UNIQUE','CHECK','EXCLUDE','LIKE'):
                cols[norm(cm.group(1))] = cm.group(2)
    # ALTER TABLE x ADD COLUMN y type
    for m in re.finditer(r'ALTER TABLE\s+(?:IF EXISTS\s+)?(?:public\.)?"?([A-Za-z_]\w*)"?\s+ADD COLUMN\s+(?:IF NOT EXISTS\s+)?"?([A-Za-z_]\w*)"?\s+([A-Za-z_][\w".()\[\]]*(?:\[\])?)', s):
        tables.setdefault(norm(m.group(1)), {})[norm(m.group(2))] = m.group(3)
    # ALTER TABLE x ALTER COLUMN y TYPE t / SET DATA TYPE
    for m in re.finditer(r'ALTER TABLE\s+"?([A-Za-z_]\w*)"?\s+ALTER COLUMN\s+"?([A-Za-z_]\w*)"?\s+(?:SET DATA\s+)?TYPE\s+([A-Za-z_][\w".()\[\]]*)', s, re.I):
        tables.setdefault(norm(m.group(1)), {})[norm(m.group(2))] = m.group(3)
    # RENAME COLUMN
    for m in re.finditer(r'ALTER TABLE\s+"?([A-Za-z_]\w*)"?\s+RENAME COLUMN\s+"?([A-Za-z_]\w*)"?\s+TO\s+"?([A-Za-z_]\w*)"?', s, re.I):
        t = norm(m.group(1)); old = norm(m.group(2)); new = norm(m.group(3))
        if t in tables and old in tables[t]:
            tables[t][new] = tables[t].pop(old)

def base_type(t):
    t = t.strip().rstrip('[]').strip('"')
    tl = t.lower()
    mp = {'text':'text','varchar':'text','character varying':'text','char':'text',
          'uuid':'uuid','bigint':'int8','bigserial':'int8','integer':'int4','int':'int4',
          'serial':'int4','smallint':'int2','boolean':'bool','timestamp':'timestamp',
          'timestamptz':'timestamptz','timestamp(3)':'timestamp','date':'date',
          'jsonb':'jsonb','json':'jsonb','double precision':'float8','float8':'float8',
          'real':'float4','bytea':'bytea'}
    return mp.get(tl, tl)

# global col -> set of (table, type)
colmap = {}
for t, cols in tables.items():
    for c, ty in cols.items():
        colmap.setdefault(c, {}).setdefault(base_type(ty), set()).add(t)

def resolve_col(col, query_sql):
    """Resolve column type: unique type globally, else via alias->table in query."""
    c = norm(col)
    cands = colmap.get(c, {})
    if len(cands) == 1:
        return next(iter(cands))
    if not cands:
        return None
    # try alias-qualified resolution
    # find alias for the operand
    return None

ANY_RE = re.compile(
    r'("?[A-Za-z_][\w"]*"?\.)?"?([A-Za-z_][\w]*)"?\s*(=|!=|<>)\s*(ANY|ALL)\s*\(\s*\$(\d+)\s*\)')

def process(path):
    s = open(path).read()
    changed = False
    def sub(m):
        nonlocal changed
        qual, col, op, kw, num = m.groups()
        # resolve type
        ty = None
        if qual:
            tbl = qual.rstrip('.').strip('"')
            # alias or table name — look in query FROM/JOIN
            qm = re.search(r'(?i)\b(?:FROM|JOIN|UPDATE|INTO)\s+"?([A-Za-z_]\w*)"?\s+(?:AS\s+)?' + re.escape(qual.rstrip('.').strip('"')) + r'\b', s)
            target = None
            if tbl.lower() in tables:
                target = tbl.lower()
            elif qm:
                target = norm(qm.group(1))
            if target and norm(col) in tables.get(target, {}):
                ty = base_type(tables[target][norm(col)])
        if ty is None:
            cands = colmap.get(norm(col), {})
            if len(cands) == 1:
                ty = next(iter(cands))
            elif len(cands) > 1:
                # pick most common type (text/uuid bias by column name heuristics)
                ty = sorted(cands, key=lambda x: -len(colmap[norm(col)][x]))[0]
        if ty is None:
            print('  UNRESOLVED', path, m.group(0))
            return m.group(0)
        changed = True
        q = qual or ''
        return f'{q}"{col}" {op} {kw}(${num}::{ty}[])'
    s2 = ANY_RE.sub(sub, s)
    if changed:
        open(path, 'w').write(s2)
        print('fixed', path)

for f in glob.glob(f'{BASE}/macrodb/*.sql') + glob.glob(f'{BASE}/emaildb/*.sql'):
    if 'ANY($' in open(f).read() or 'ALL($' in open(f).read():
        process(f)
