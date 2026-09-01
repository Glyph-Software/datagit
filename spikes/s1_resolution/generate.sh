#!/usr/bin/env bash
# S1 dataset generation. THROWAWAY spike code (PLAN.md Phase 0).
#
# Builds the table and its data first, then the indexes — building indexes last
# is several times faster than maintaining them through 50M inserts.
set -euo pipefail

SVC="${SVC:-pg17}"
KEYS="${KEYS:-10000000}"       # live keys on main
VERSIONS="${VERSIONS:-5}"      # versions per key (4 closed + 1 open)
OVERLAY="${OVERLAY:-200000}"   # overlay rows per branch
CHUNK="${CHUNK:-1000000}"      # keys per insert statement
BRANCHES="${BRANCHES:-7}"

psql() { docker compose exec -T "$SVC" psql -U datagit -v ON_ERROR_STOP=1 "$@"; }

echo "=== schema"
psql -q < "$(dirname "$0")/setup.sql"

echo "=== main: $KEYS keys x $VERSIONS versions"
start=$(date +%s)
for ((lo=0; lo<KEYS; lo+=CHUNK)); do
  hi=$((lo+CHUNK)); (( hi > KEYS )) && hi=$KEYS
  psql -q -c "
    INSERT INTO datagit_v_products
      (branch_id, seq_from, seq_to, op, commit_id, changed_cols,
       sku, name, category, price, updated_at)
    SELECT
      bid(0),
      -- Contiguous, non-overlapping intervals per key, with per-key jitter so
      -- boundaries are not aligned across keys.
      v.i * 200 + (k.k % 190),
      CASE WHEN v.i = $((VERSIONS-1))
           THEN 9223372036854775807
           ELSE (v.i + 1) * 200 + (k.k % 190) END,
      CASE WHEN v.i = 0 THEN 1 ELSE 2 END,
      int8send(k.k) || int8send(k.k) || int8send(v.i::bigint) || int8send(0::bigint),
      '\\x0e'::bytea,
      'sku-' || lpad(k.k::text, 8, '0'),
      'product ' || k.k,
      -- 1000 distinct categories => ~0.1% selectivity per value, which is the
      -- selectivity PLAN.md S1 specifies for the filtered scan.
      'cat-' || lpad((k.k % 1000)::text, 4, '0'),
      ((k.k % 90000) + 1000)::numeric / 100,
      timestamptz '2020-01-01' + (k.k % 2000) * interval '1 day'
    FROM generate_series($lo, $hi - 1) AS k(k)
    CROSS JOIN generate_series(0, $((VERSIONS-1))) AS v(i);
  "
  printf '  keys %d/%d  (%ds)\n' "$hi" "$KEYS" "$(( $(date +%s) - start ))"
done

echo "=== branches b1..b$BRANCHES: $OVERLAY overlay rows each"
for ((b=1; b<=BRANCHES; b++)); do
  psql -q -c "
    INSERT INTO datagit_v_products
      (branch_id, seq_from, seq_to, op, commit_id, changed_cols,
       sku, name, category, price, updated_at)
    SELECT
      bid($b), 1, 9223372036854775807,
      -- 10% tombstones: a branch-level delete must mask the inherited row, and
      -- that is exactly the §7.3 hazard the correctness check probes.
      CASE WHEN k.k % 10 = 0 THEN 3 ELSE 2 END,
      int8send(k.k) || int8send($b::bigint) || int8send(0::bigint) || int8send(0::bigint),
      '\\x04'::bytea,
      'sku-' || lpad(k.k::text, 8, '0'),
      'branch $b product ' || k.k,
      -- Branch edits move rows BETWEEN categories. That is what makes pushing a
      -- category filter into the resolution arms unsafe (§7.3).
      'cat-' || lpad(((k.k + $b * 37) % 1000)::text, 4, '0'),
      ((k.k % 90000) + 5000)::numeric / 100,
      timestamptz '2026-01-01'
    FROM generate_series(($b - 1) * $OVERLAY, $b * $OVERLAY - 1) AS k(k);
  "
  echo "  b$b done"
done

echo "=== indexes (DESIGN.md §5.2)"
psql -q -c "CREATE INDEX v_products_resolve ON datagit_v_products (branch_id, sku, seq_from DESC);"
echo "  resolve"
psql -q -c "CREATE INDEX v_products_range   ON datagit_v_products (branch_id, seq_from, seq_to);"
echo "  range"
psql -q -c "CREATE INDEX v_products_commit  ON datagit_v_products (commit_id);"
echo "  commit"
psql -q -c "CREATE INDEX v_products_session ON datagit_v_products (session_id) WHERE session_id IS NOT NULL;"
echo "  session"

echo "=== analyze"
psql -q -c "ANALYZE datagit_v_products;"

psql -c "
  SELECT count(*) AS rows,
         pg_size_pretty(pg_table_size('datagit_v_products'))   AS heap,
         pg_size_pretty(pg_indexes_size('datagit_v_products')) AS indexes,
         pg_size_pretty(pg_total_relation_size('datagit_v_products')) AS total
  FROM datagit_v_products;"
echo "=== done in $(( $(date +%s) - start ))s"
