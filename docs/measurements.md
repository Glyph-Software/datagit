# Measured performance

These are **measurements**, not targets. DESIGN.md §14.1 carries targets, and
where a number here contradicts one there, this file wins and §14.1 should be
corrected.

Reproduce with:

```bash
make test-bench
```

## What the gates are for

Each gate asserts a **budget**, and a build that exceeds one fails. The budgets
are several times the measured figure on purpose: these run on developer laptops
and shared CI, and a gate that fails on noise gets disabled and then deleted.

They catch a **regression in kind** — an index that stopped being used, a
resolution that became quadratic in chain depth, a diff that started scanning the
table. They are not a substitute for §14.1 measurement on representative
hardware, and a gate passing does not mean the system is fast enough for a given
workload.

## Figures

Recorded 2026-09-02 on an Apple-silicon laptop, engines in Docker with the
`docker-compose.yml` settings, 2000-row fixture table. Each figure is one run,
so treat the small ones as "under 50ms" rather than as precise.

| Gate | PostgreSQL 17 | PostgreSQL 16 | MySQL 8.4 | Budget |
|---|---|---|---|---|
| Commit 2000 rows, one batch | 1.14s | 1.14s | 1.97s | 30s |
| Resolve a branch, 2000 rows | 3ms | 3ms | 4ms | 15s |
| Filtered branch read | 18ms | 18ms | 3ms | 10s |
| Diff 50 of 2000 rows | 15ms | 11ms | 31ms | 5s |
| History of one key, 21 versions | 1ms | 1ms | 1ms | 3s |
| Blame one key | <1ms | <1ms | 1ms | 3s |
| Resolve at chain depth 8 | 1ms | 1ms | 2ms | 15s |

## What the figures say

**MySQL is slower on writes and comparable on reads.** The batch commit is about
1.7× PostgreSQL's. That is a performance gap, not a capability difference, and
per the §4.3 rule it is published here rather than recorded in the capability
matrix.

**MySQL is not slower on the resolution query.** M5 anticipated that
`ROW_NUMBER()` might trail `DISTINCT ON`. At this scale it does not, and the
filtered read is faster. Whether that holds at S1's 51.4M versions is not
answered by these gates and should not be inferred from them.

**Diff costs the size of the change.** Fifty changed rows in a 2000-row table
diff in milliseconds, which is the §8.1 claim behaving as designed. The gate
asserts the row count as well as the time, so a diff that got fast by returning
less would fail rather than pass.

**Chain depth is not yet a cost at depth 8.** Resolution at the §18 cap is
indistinguishable from resolution at depth 1 here. The cap exists because the
cost grows with depth at scale, and this fixture is too small to show it. The
observability counter for chain depth is the production signal, not this gate.

## What is not measured here

- **Scale.** The Phase 0 spikes measured at 51.4M versions; these gates run at
  thousands. Findings F7 and F9 — the per-column index turning a 20-second
  filtered read into milliseconds, and per-arm ordering bounding the work —
  are invisible at this size. They are recorded in `docs/phase0/findings.md`.
- **Concurrency.** The ~850 commits/s ref-lock ceiling (§11.3) is a Phase 0
  measurement and is not re-measured here.
- **Storage at rest.** 3.33× measured in S5, unchanged by anything since.
