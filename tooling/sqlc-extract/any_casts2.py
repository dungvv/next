#!/usr/bin/env python3
"""Fix wrongly-cast ANY params: re-resolve column types using tables actually
present in each query (alias -> table from FROM/JOIN/UPDATE clauses)."""
import re, glob

SCHEMA_DIR = '/tmp/macroschema'
BASE = '/home/dungvv/projects/next/sqlc'

tables = {}
def norm(t): return t.strip('"').lower()

create_re = re.compile(r'CREATE TABLE\s+(?:IF NOT EXISTS\s+)?(?:public\.)?"?([A-Za-z_][\w]*)"?\s*\((.*?)\)\s*;', re.S | re.I)
for f in sorted(glob.glob(f'{SCHEMA_DIR}/*.sql')):
    s = open(f).read()
    for m in create_re.finditer(s):
        tname = norm(m.group(1))
        body = m.group(2)
        cols = tables.setdefault(tname, {})
        depth = 0; cur = ''; parts = []
        for ch in body:
            if ch == '(': depth += 1
            elif ch == ')': depth -= 1
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
    for m in re.finditer(r'ALTER TABLE\s+(?:IF EXISTS\s+)?(?:public\.)?"?([A-Za-z_]\w*)"?\s+ADD COLUMN\s+(?:IF NOT EXISTS\s+)?"?([A-Za-z_]\w*)"?\s+([A-Za-z_][\w".()\[\]]*(?:\[\])?)', s):
        tables.setdefault(norm(m.group(1)), {})[norm(m.group(2))] = m.group(3)
    for m in re.finditer(r'ALTER TABLE\s+"?([A-Za-z_]\w*)"?\s+ALTER COLUMN\s+"?([A-Za-z_]\w*)"?\s+(?:SET DATA\s+)?TYPE\s+([A-Za-z_][\w".()\[\]]*)', s, re.I):
        tables.setdefault(norm(m.group(1)), {})[norm(m.group(2))] = m.group(3)
    for m in re.finditer(r'ALTER TABLE\s+"?([A-Za-z_]\w*)"?\s+RENAME COLUMN\s+"?([A-Za-z_]\w*)"?\s+TO\s+"?([A-Za-z_]\w*)"?', s, re.I):
        t = norm(m.group(1)); old = norm(m.group(2)); new = norm(m.group(3))
        if t in tables and old in tables[t]:
            tables[t][new] = tables[t].pop(old)
    for m in re.finditer(r'ALTER TABLE\s+"?([A-Za-z_]\w*)"?\s+DROP COLUMN\s+"?([A-Za-z_]\w*)"?', s, re.I):
        tables.get(norm(m.group(1)), {}).pop(norm(m.group(2)), None)

def base_type(t):
    t = t.strip().rstrip('[]').strip('"')
    tl = t.lower()
    mp = {'text':'text','varchar':'text','character varying':'text','character varying(255)':'text',
          'char':'text','uuid':'uuid','bigint':'int8','bigserial':'int8','integer':'int4','int':'int4',
          'serial':'int4','smallint':'int2','boolean':'bool','timestamp':'timestamp',
          'timestamptz':'timestamptz','timestamp(3)':'timestamp','timestamp with time zone':'timestamptz',
          'date':'date','jsonb':'jsonb','json':'jsonb','double precision':'float8','float8':'float8',
          'real':'float4','bytea':'bytea'}
    # normalize parens
    tl = re.sub(r'\(\d+\)', '', tl).strip()
    if tl.endswith(' with time zone'): tl = 'timestamptz'
    if tl.endswith('[]'): tl = tl[:-2] + '[]'
    return mp.get(tl, tl)

# Split query file into per-query blocks
ANY_CAST_RE = re.compile(
    r'("?[A-Za-z_][\w"]*"?\.)?"?([A-Za-z_][\w]*)"?\s*(=|!=|<>)\s*(ANY|ALL)\s*\(\s*\$(\d+)::([a-z_0-9]+)\[\]\s*\)')

def tables_in_scope(block, pos):
    """Return alias->table map for the statement containing pos.
    Approximation: scan whole block for FROM/JOIN/UPDATE/INTO/DELETE FROM items."""
    amap = {}
    for m in re.finditer(
        r'(?i)\b(?:FROM|JOIN|UPDATE|INTO|USING)\s+(?:public\.)?"?([A-Za-z_][\w]*)"?\s*(?:AS\s+)?("?[a-zA-Z_][\w"]*)?',
        block):
        tbl = norm(m.group(1))
        alias = (m.group(2) or '').strip('"')
        amap.setdefault(tbl, tbl)
        if alias and alias.lower() not in ('on','where','set','values','returning','order','group','limit','left','right','inner','cross','join','union','select','and','or','not','as','using','full','lateral'):
            amap[norm(alias)] = tbl
    return amap

def resolve(block, qual, col):
    amap = tables_in_scope(block, 0)
    c = norm(col)
    if qual:
        a = norm(qual.rstrip('.'))
        tbl = amap.get(a, a)
        if tbl in tables and c in tables[tbl]:
            return base_type(tables[tbl][c])
        # qual may be a table not aliased
        return None
    # unqualified: candidate tables = all tables/cte targets in scope having the col
    cand = set()
    for tbl in set(amap.values()):
        if c in tables.get(tbl, {}):
            cand.add(base_type(tables[tbl][c]))
    if len(cand) == 1:
        return next(iter(cand))
    if len(cand) > 1:
        return 'AMBIG:' + '|'.join(sorted(cand))
    # column might come from a subquery/select-list — fall back to unique global
    global_cand = set()
    for t, cols in tables.items():
        if c in cols:
            global_cand.add(base_type(cols[c]))
    if len(global_cand) == 1:
        return next(iter(global_cand))
    return None

for path in glob.glob(f'{BASE}/macrodb/*.sql') + glob.glob(f'{BASE}/emaildb/*.sql'):
    s = open(path).read()
    # split into query blocks at '-- name:' boundaries
    marks = list(re.finditer(r'^-- name:', s, re.M))
    out = []
    last = 0
    changed = False
    for i, mk in enumerate(marks + [None]):
        if mk is None:
            seg = s[last:]
            end = len(s)
        else:
            seg = s[last:mk.start()]
        blocks_end = marks[i].start() if mk else len(s)
        block = s[last:blocks_end]
        def sub(m):
            nonlocal_changed = [False]
            qual, col, op, kw, num, cur_ty = m.groups()
            ty = resolve(block, qual, col)
            if ty is None:
                print('UNRESOLVED', path, m.group(0))
                return m.group(0)
            if ty.startswith('AMBIG:'):
                print('AMBIG', path, m.group(0), ty)
                return m.group(0)
            if ty != cur_ty:
                nonlocal_changed[0] = True
            q = qual or ''
            return f'{q}"{col}" {op} {kw}(${num}::{ty}[])'
        def sub2(m):
            r = sub(m)
            if r != m.group(0):
                sub2.changed = True
            return r
        newblock = ANY_CAST_RE.sub(sub2, block)
        if getattr(sub2, 'changed', False):
            changed = True
        out.append(newblock)
        if mk is None:
            break
        last = blocks_end
    if changed:
        open(path, 'w').write(''.join(out))
        print('retyped', path)
print('done')
