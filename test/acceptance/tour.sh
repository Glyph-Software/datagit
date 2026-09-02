#!/usr/bin/env bash
# Acceptance test: the README's tour, run verbatim (PLAN.md W5).
#
# The point is that documentation stays true. If the README shows a command, this
# runs it and checks the outcome; if the command changes, this breaks.
set -euo pipefail

DSN_BASE="${DATAGIT_TEST_DSN:-postgres://datagit:datagit@localhost:55417/datagit}"
SCHEMA="tour_$$"
BIN="${BIN:-./bin/datagit}"

export DATAGIT_DSN="${DSN_BASE}?search_path=${SCHEMA}"
export DATAGIT_REPO=catalog
export DATAGIT_AUTHOR="arun@example.com"

psql() { docker compose exec -T pg17 psql -U datagit -v ON_ERROR_STOP=1 "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { printf '\n=== %s\n' "$*"; }

cleanup() { psql -q -c "DROP SCHEMA IF EXISTS ${SCHEMA} CASCADE" >/dev/null 2>&1 || true; }
trap cleanup EXIT

step "fixture"
psql -q -c "CREATE SCHEMA ${SCHEMA}"
psql -q -c "
  SET search_path TO ${SCHEMA};
  CREATE TABLE products (
    sku text PRIMARY KEY, name text, category text,
    price numeric(12,2), updated_at timestamptz);
  INSERT INTO products VALUES
    ('TENT-4P','Four-person tent','outdoor',249.00,'2026-03-02'),
    ('STOVE-V1','Camp stove','outdoor',89.50,'2026-03-02'),
    ('MUG-01','Enamel mug','kitchen',12.00,'2026-03-02');"

step "README: repo init and track"
$BIN repo init catalog
$BIN track products --mode versioned

step "README: branch, change on the branch, review, merge"
$BIN branch create q4-pricing
$BIN commit products --branch q4-pricing --pk sku=TENT-4P \
  --set "price=268.92" -m "Q4 outdoor price increase (approved in FIN-2291)" --ref FIN-2291

# The live table must NOT have moved: branch work is invisible to direct readers.
live=$(psql -tAc "SELECT price FROM ${SCHEMA}.products WHERE sku='TENT-4P'" | tr -d ' \r')
[ "$live" = "249.00" ] || fail "a branch commit changed the live table (got $live, want 249.00)"

$BIN branch protect main --approvals 1
$BIN proposal create --from q4-pricing --into main --title "Q4 outdoor pricing"

# The author cannot approve their own proposal on a protected branch.
if DATAGIT_AUTHOR=arun@example.com $BIN proposal approve --id 1 2>/dev/null; then
  fail "self-approval on a protected branch was permitted"
fi
DATAGIT_AUTHOR=maya@example.com $BIN proposal approve --id 1 -m "checked against FIN-2291"
DATAGIT_AUTHOR=maya@example.com $BIN proposal merge --id 1 --table products

# Now the live table IS the new state, for every direct reader at once.
live=$(psql -tAc "SELECT price FROM ${SCHEMA}.products WHERE sku='TENT-4P'" | tr -d ' \r')
[ "$live" = "268.92" ] || fail "the merged proposal did not reach the live table (got $live)"

step "README: blame"
out=$($BIN blame products --pk sku=TENT-4P)
echo "$out"
# On main the price arrived via the MERGE commit, not the branch commit: that is
# the honest answer to "when did this value get here, on this branch".
echo "$out" | grep -q "Q4 outdoor pricing" || fail "blame does not attribute the price change"
# An unchanged column must NOT be attributed to the latest commit.
echo "$out" | grep "^name" | grep -q "repository created" \
  || fail "blame attributed an unchanged column to the latest commit"

step "README: history and revert"
$BIN history products --pk sku=TENT-4P
CID=$(psql -tAc "SELECT encode(id,'hex') FROM ${SCHEMA}.datagit_commit ORDER BY seq DESC LIMIT 1" | tr -d ' \r\n')
$BIN revert "$CID" --table products -m "Roll back the Q4 increase"
live=$(psql -tAc "SELECT price FROM ${SCHEMA}.products WHERE sku='TENT-4P'" | tr -d ' \r')
[ "$live" = "249.00" ] || fail "revert did not restore the live table (got $live)"

# A revert ADDS history; it never erases.
n=$(psql -tAc "SELECT count(*) FROM ${SCHEMA}.datagit_commit" | tr -d ' \r')
[ "$n" -ge 4 ] || fail "revert appears to have rewritten history (only $n commits)"

step "README: materialize a branch for unrestricted SQL"
$BIN materialize q4-pricing --into "${SCHEMA}_mat"
agg=$(psql -tAc "SELECT max(price) FROM ${SCHEMA}_mat.products" | tr -d ' \r')
[ "$agg" = "268.92" ] || fail "materialized branch is wrong (got $agg)"
psql -q -c "DROP SCHEMA ${SCHEMA}_mat CASCADE"

step "README: verify"
$BIN verify

step "operations: prune, gc"
$BIN prune --table products --keep-commits 2
$BIN gc
$BIN verify

step "schema changes reach main as a plan, not as a surprise"
# DESIGN.md §10.4. This is the deliberately un-Git-like part: a data merge into
# main applies immediately, a SHAPE change does not, because applications read
# the live table directly and a column that appears or vanishes mid-query has no
# rollout window.
$BIN branch create shape --from main
DATAGIT_BRANCH=shape $BIN schema add-column --table products \
  --column margin_pct --type "numeric(5,2)"

cols_before=$(psql -tAc "SELECT count(*) FROM information_schema.columns
  WHERE table_schema='${SCHEMA}' AND table_name='products'" | tr -d ' \r')
[ "$cols_before" = "5" ] || fail "a branch schema change altered the live table (${cols_before} columns)"
echo "  ok    the live table still has 5 columns while the branch has 6"

$BIN proposal create --from shape --into main --title "add margin_pct"
# NR==2 skips the header row. A failure here is a failure of the tour, so there
# is no `|| true` hiding it.
PID=$($BIN proposal list | awk 'NR==2{print $1}')
[ -n "$PID" ] || fail "proposal create produced no proposal"
# main was protected earlier in the tour, so a schema change needs review like
# any other. The author's own approval does not count.
DATAGIT_AUTHOR="maya@example.com" $BIN proposal approve --id "$PID" -m "shape looks right"
DATAGIT_AUTHOR="maya@example.com" $BIN proposal merge --id "$PID" --table products

cols_mid=$(psql -tAc "SELECT count(*) FROM information_schema.columns
  WHERE table_schema='${SCHEMA}' AND table_name='products'" | tr -d ' \r')
[ "$cols_mid" = "5" ] || fail "the merge changed the live table immediately (${cols_mid} columns)"
echo "  ok    merging did not change the live table's shape"

$BIN migration list
MID=$($BIN migration list | awk 'NR==1{print $1}')
[ -n "$MID" ] || fail "merging a schema change produced no migration plan"
$BIN migration show "$MID"
DATAGIT_AUTHOR="maya@example.com" $BIN migration apply "$MID"

cols_after=$(psql -tAc "SELECT count(*) FROM information_schema.columns
  WHERE table_schema='${SCHEMA}' AND table_name='products'" | tr -d ' \r')
[ "$cols_after" = "6" ] || fail "applying the plan did not add the column (${cols_after} columns)"
echo "  ok    the column arrived when someone chose the moment"

step "the exit door: untrack leaves the live table intact"
# `| head` would SIGPIPE the exporter, and pipefail would end the script.
$BIN export products > /tmp/datagit_export_$$.jsonl
head -2 /tmp/datagit_export_$$.jsonl
grep -q '"kind":"commit"' /tmp/datagit_export_$$.jsonl || fail "export is missing commits"
rm -f /tmp/datagit_export_$$.jsonl
$BIN untrack products
n=$(psql -tAc "SELECT count(*) FROM ${SCHEMA}.products" | tr -d ' \r')
[ "$n" = "3" ] || fail "untrack changed the live table (got $n rows)"

printf '\n=== all README steps behaved as documented\n'
