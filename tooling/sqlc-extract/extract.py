#!/usr/bin/env python3
"""Extract sqlx queries from Rust sources into sqlc-annotated .sql files."""
import os, re, sys, json

def camel(s):
    return ''.join(w[:1].upper() + w[1:] for w in re.split(r'[_\-\s]+', s) if w)

RAW_RE = re.compile(r'r(#+)"')

def find_raw_string(text, start):
    """Find next Rust raw string starting at/after `start`. Returns (sql, end) or None."""
    m = RAW_RE.search(text, start)
    candidates = []
    while m:
        hashes = m.group(1)
        open_end = m.end()
        close = '"' + hashes
        close_idx = text.find(close, open_end)
        if close_idx == -1:
            return None
        candidates.append((m.start(), open_end, close_idx + len(close), text[open_end:close_idx]))
        m = RAW_RE.search(text, close_idx + len(close))
    if candidates:
        st, se, ce, sql = candidates[0]
        return sql, ce, st
    return None

def find_string_literal(text, start):
    """Find next SQL literal: raw string r#".."# or normal "..". Returns (sql, end, pos)."""
    raw = RAW_RE.search(text, start)
    norm = re.compile(r'"').search(text, start)
    if raw and (not norm or raw.start() < norm.start()):
        hashes = raw.group(1)
        open_end = raw.end()
        close = '"' + hashes
        close_idx = text.find(close, open_end)
        if close_idx == -1:
            return None
        return text[open_end:close_idx], close_idx + len(close), raw.start()
    if norm:
        # normal string: scan for closing unescaped quote
        i = norm.end()
        buf = []
        while i < len(text):
            c = text[i]
            if c == '\\':
                buf.append(text[i:i+2]); i += 2; continue
            if c == '"':
                s = ''.join(buf)
                s = s.replace('\\"', '"').replace('\\n', '\n').replace('\\t', '\t').replace('\\\\', '\\')
                return s, i + 1, norm.start()
            buf.append(c); i += 1
    return None

MACRO_RE = re.compile(r'sqlx::(query|query_as|query_scalar)(_with)?!?(::\s*<[^>]*>)?\s*\(')
# also query!{ ... } brace form unlikely; skip.

def enclosing_fn(text, pos):
    """Find the enclosing fn name by scanning backwards for 'fn name'."""
    seg = text[:pos]
    fns = list(re.finditer(r'\bfn\s+([a-zA-Z_][a-zA-Z0-9_]*)', seg))
    if not fns:
        return None
    return fns[-1].group(1)

def fetch_mode(text, qend):
    """Look ahead for .fetch_* / .execute to determine sqlc annotation.
    Window ends at the next sqlx:: query or next fn definition."""
    lookahead = text[qend:qend+8000]
    nextq = MACRO_RE.search(lookahead)
    nextfn = re.search(r'\bfn\s+[a-zA-Z_]', lookahead)
    cut = min([x.start() for x in (nextq, nextfn) if x], default=len(lookahead))
    lookahead = lookahead[:cut]
    m = re.search(r'\.(fetch_one|fetch_optional|fetch_all|fetch|execute)\s*\(', lookahead)
    if not m:
        return None
    kind = m.group(1)
    return {'fetch_one': ':one', 'fetch_optional': ':one', 'fetch_all': ':many',
            'fetch': ':many', 'execute': ':exec'}[kind]

def is_dynamic(sql):
    return ('{' in sql and '}' in sql) or '${' in sql

def strip_test_modules(text):
    """Remove `#[cfg(test)] mod ... { ... }` blocks via brace matching."""
    out = text
    for m in list(re.finditer(r'#\[cfg\(test\)\][^{]*\bmod\s+\w+\s*\{', text)):
        brace = text.find('{', m.end() - 1)
        if brace == -1:
            continue
        depth = 0
        i = brace
        # naive brace matching; strings may confound it but test mods rarely
        # contain unbalanced braces outside raw strings
        while i < len(text):
            c = text[i]
            if c == '{':
                depth += 1
            elif c == '}':
                depth -= 1
                if depth == 0:
                    break
            i += 1
        out = out.replace(text[m.start():i + 1], '')
    return out


def module_name(crate_root, path):
    rel = os.path.relpath(path, crate_root)
    parts = rel[:-3].split(os.sep)  # strip .rs
    if parts[-1] == 'mod':
        parts = parts[:-1]
    return '_'.join(parts)

def extract_crate(crate_src, out_dir, pkg):
    used = {}        # name -> count
    sql2name = {}    # sql -> name (dedupe)
    skipped = []
    stats = {'files': 0, 'queries': 0}
    out_files = {}
    for dirpath, _, files in os.walk(crate_src):
        for fn in sorted(files):
            if not fn.endswith('.rs'):
                continue
            if fn in ('test.rs', 'tests.rs'):
                continue
            path = os.path.join(dirpath, fn)
            text = open(path, encoding='utf8').read()
            text = strip_test_modules(text)
            stats['files'] += 1
            mod = module_name(crate_src, path)
            base = fn[:-3]
            queries = []
            for m in MACRO_RE.finditer(text):
                lit = find_string_literal(text, m.end())
                if not lit:
                    skipped.append((path, enclosing_fn(text, m.start()) or '?', 'no literal SQL (const/format)'))
                    continue
                sql, lit_end, lit_start = lit
                # ensure literal is the first arg (allow type ident for query_as!: `Type,`)
                between = text[m.end():lit_start].strip()
                kind = m.group(1)
                if kind in ('query_as', 'query_scalar'):
                    if between == '':
                        ok = True
                    else:
                        # first arg to query_as!/query_scalar! is a type — allow any ident
                        ok = re.fullmatch(r'[A-Za-z_][A-Za-z0-9_:<>]*\s*,', between) is not None
                else:
                    ok = between == ''
                if not ok:
                    skipped.append((path, enclosing_fn(text, m.start()) or '?', 'literal not first arg'))
                    continue
                fnname = enclosing_fn(text, m.start()) or base
                if fnname.startswith(('test_', 'tests_')):
                    continue
                mode = fetch_mode(text, lit_end)
                if mode is None:
                    skipped.append((path, fnname, 'no fetch/execute found'))
                    continue
                # note: sqlx query macros always take literal SQL; '{}'/format
                # braces inside are Postgres array literals, not format placeholders.
                if sql.strip() in sql2name:
                    continue  # dedupe identical
                name = camel(fnname)
                if name in used:
                    used[name] += 1
                    name = f"{name}{used[name]}"
                else:
                    used[name] = 1
                sql2name[sql.strip()] = name
                body = sql.strip()
                if not body.endswith(';'):
                    body += ';'
                queries.append(f"-- name: {name} {mode}\n{body}\n")
                stats['queries'] += 1
            if queries:
                out_files[mod] = out_files.get(mod, []) + queries
    os.makedirs(out_dir, exist_ok=True)
    for mod, qs in out_files.items():
        fname = re.sub(r'[^a-zA-Z0-9_]', '_', mod) + '.sql'
        with open(os.path.join(out_dir, fname), 'w') as f:
            f.write('\n'.join(q + '\n' for q in qs))
    return stats, skipped, len(used)

if __name__ == '__main__':
    src, out, pkg = sys.argv[1], sys.argv[2], sys.argv[3]
    stats, skipped, nq = extract_crate(src, out, pkg)
    print(json.dumps(stats))
    for s in skipped:
        print('SKIP', *s)
