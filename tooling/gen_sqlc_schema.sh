#!/usr/bin/env bash
# Generate a flattened schema dir for sqlc from the sqlx migration set.
#
# sqlc cannot parse:
#   - *.down.sql files (applied in filename order, corrupting the catalog)
#   - DO $$ ... $$ blocks (0001_baseline.sql wraps ~80 CREATE TABLEs in one;
#     crm_cleanup_tables.sql wraps a CREATE TYPE)
#   - plpgsql CREATE FUNCTION / CREATE TRIGGER bodies
#
# This script produces sqlc/schema/*.sql in migration order with those
# constructs stripped/unwrapped. Re-run whenever migrations change.
set -euo pipefail

SRC=crates/macro_db_client/migrations
OUT=sqlc/schema
rm -rf "$OUT"
mkdir -p "$OUT"

for f in "$SRC"/*.sql; do
  base=$(basename "$f")
  case "$base" in
    *.down.sql) continue ;;
  esac
  out="$OUT/$base"

  perl -0pe '
    # Unwrap DO $$ blocks: keep the body between BEGIN and END only when it is
    # pure DDL (CREATE/ALTER/DROP/COMMENT). Drop backfill blocks containing
    # loops/DML, EXCEPTION clauses, and IF/END IF guards.
    s{DO\s*\$\$.*?BEGIN(.*?)END\s*\$\$\s*;}{
      my $b = $1;
      $b =~ s{EXCEPTION.*}{}is;
      $b =~ s{IF\s+.*?THEN}{}gis;
      $b =~ s{ELSIF\s+.*?THEN}{}gis;
      $b =~ s{END\s+IF\s*;}{}gis;
      $b =~ /\b(LOOP|WHILE|PERFORM|EXIT|RETURN|INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|FOR\s+\w+\s+IN)\b/is ? "" : $b
    }gise;

    # Strip plpgsql constructs the catalog builder cannot parse.
    s/CREATE\s+(OR\s+REPLACE\s+)?FUNCTION.*?LANGUAGE\s+\w+\s*;//gis;
    s/CREATE\s+(OR\s+REPLACE\s+)?TRIGGER\b[^;]*;//gis;
    s/CREATE\s+(OR\s+REPLACE\s+)?RULE\b[^;]*;//gis;
  ' "$f" > "$out"
done

echo "flattened schema written to $OUT ($(ls "$OUT" | wc -l) files)"
