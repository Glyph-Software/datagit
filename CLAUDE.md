# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

**Complete through M7, on both engines.** Phase 0 finished with nothing unrun.
The full integration suite runs against PostgreSQL 16, PostgreSQL 17, and MySQL
8.4 — the same suite, not a parallel one per engine.

| File | Role |
|---|---|
| [DESIGN.md](DESIGN.md) | **Source of truth.** 20 sections. Section references (§7.3, §9.2) throughout the code point here. |
| [PLAN.md](PLAN.md) | Build sequence with per-milestone status of what is done and what is outstanding. |
| [docs/phase0/findings.md](docs/phase0/findings.md) | The eleven findings that changed the design, with measurements. |
| [README.md](README.md) | Overview, trade-offs, roadmap. |

Before writing implementation code, read DESIGN.md §5–§10. Before proposing an
architectural change, read §19 — it records what was already considered and
rejected. **Before touching resolution, merge, or the sidecar, read the Phase 0
findings**: five are correctness bugs already found and fixed, and the code
carries `finding Fn` markers where they apply.

## Layout

```
internal/core       frozen value/mask/change types (data only, no logic)
internal/hash       the commit hash chain, frozen as datagit.commit.v1
internal/adapter    engine boundary + Caps; postgres/ and mysql/
internal/db         connection boundary: $N->? rebinding, identifier quoting
internal/pg         PostgreSQL pool (pgx)
internal/my         MySQL pool (database/sql), with the mechanical rewrites
internal/connect    picks the engine from a DSN, returns a MATCHED pool+adapter
internal/catalog    control schema and sidecar naming
internal/store      write path, resolution, diff, blame, merge, branches,
                    sessions, proposals, retention, purge
internal/schemaeng  schema versioning, diff, merge, migration planning
internal/crypto     crypto-shredding, anchoring, commit signing
internal/server     gRPC handlers, API-key auth, and the in-process REST surface
internal/obs        metrics, including resolution chain depth
internal/model      REFERENCE implementation — test-only, never imported by
                    production code
cmd/datagit         the CLI, which is the reference client
cmd/datagitd        the stateless server: gRPC, optional REST, separate admin port
sdk/python          Python SDK: generated stubs + ergonomic layer
sdk/typescript      TypeScript SDK: the same
test/property       the differential harness (primary correctness evidence)
test/integration    the SAME suite against every supported engine
test/bench          performance gates that FAIL a budget, not just report
test/acceptance     the README tour, run verbatim
spikes/             throwaway Phase 0 code
```

## What DataGit is

A stateless Go service between an application and its existing PostgreSQL or MySQL database, adding Git-style version control (commits, branches, cell-level three-way merge, time travel, blame) to selected tables **in place**. Rows stay in the user's database. DataGit owns writes, history, and branches — not reads on `main`. Curation (branch, review, merge for rows) is the wedge; audit-grade history is what makes it trustworthy (§1.1).

## Architecture in one pass

Three storage layers per tracked table (§5.1):

- **Live table** — the application's own table, schema unmodified. Invariant: it *is* `main@HEAD`.
- **Version sidecar** `datagit_v_<table>` — typed mirrored columns named by stable column id (`c_<id>`), one row per row-version over a half-open `seq` interval, tagged with the branch that produced it, a `changed_cols` bitmask over column ids, and a `session_id` while staged.
- **Control tables** `datagit_*` — repos, commits, refs, sessions, idempotency, proposals, conflicts, keys, journals.

Reads on `main` go straight to the live table with no DataGit hop (§7.1). A `main` commit is one RPC carrying its whole change set, applied in one transaction (§6.1). Branch reads resolve each primary key through a priority chain of `(branch, seq)` segments down to `main`, in two passes when filtered (§7.3). Diffs are interval range scans, so they cost the size of the change (§8.1). Merges are three-way against the LCA, resolved per cell via `changed_cols` (§9.2). A branch catches up with its parent via `UpdateFromParent`, which advances its fork point so the segment chain stays a tree (§9.6).

## Invariants that constrain code changes

These look like ordinary refactors and are not. Each is load-bearing, and most were learned the hard way.

1. **Never add columns, triggers, or view substitutions to a tracked live table on the happy path.** It must stay a clean, schema-unmodified materialization of `main@HEAD`, because application readers query it directly. This kills the "visibility columns" model — see §19.2.
2. **Never put DataGit on the `main` read path.** It is not a latency or availability dependency for production reads. This is the constraint the whole storage design exists to satisfy.
3. **There is no uncommitted state on `main`** (§6.1). No sidecar row on `main` ever carries the zero commit hash or a `session_id`. Sessions exist only on other branches. Anything that stages on `main` before committing reopens the audit hole the design closed.
4. **In resolution queries, filter `op <> 3` *outside* the union arms, and never push a VALUE filter into them** (§7.3). Either mistake lets an inherited parent row resurface. Filtered reads use the two-pass form. **The primary key is the one safe pushdown**, because row identity is immutable (finding F6). Measured at 51.4M versions: the wrong forms resurfaced 140,000 deleted rows and 1,400 spurious rows.
5. **A stored chain always includes the branch itself at index 0**, followed by its inherited tail, **captured at fork** (finding F1). Never re-derive it from ancestors' live fork points: absorbing a parent would silently change what descendants resolve to.
6. **`changed_cols` is a SUPERSET** (finding F2). A set bit does not imply a changed value. Masks narrow which columns are examined; every decision compares values. Disjoint masks mean clean; overlapping masks do **not** mean conflict.
7. **"Changes since base" is a chain diff taken in BOTH directions** (findings F4, F5). A branch differs from the base by lacking changes as well as by adding them.
8. **Canonical value encoding and `commit_id` construction are frozen** as `datagit.commit.v1` (§12.1). Changing them after any history exists invalidates every commit hash ever written. Guarded by golden tests.
9. **Sidecar columns are named by stable column id from the very first sidecar** (§10.5). Retrofitting ids later is a sidecar rewrite. Sidecar columns are append-only; narrowing type changes fork to a new id rather than coercing history.
10. **`internal/model` (the reference implementation) must never be imported by non-test code.** A reference model that shares code with the implementation tests nothing.
11. **Delete/modify is always a conflict** (§9.2). Multiple merge bases are **refused with candidates named**, never silently resolved (§9.1). Rebase is not offered; it rewrites hashes.
12. **Merges above the atomic apply limit are refused unless the caller opts into chunked apply**, which flags the ref `merge_in_progress` for the duration (§9.5). Never silently chunk.
13. **Purged commits are marked `integrity = 'purged'`, never re-hashed** (§13.4). Hiding the gap makes the audit trail lie.
14. **Crypto-shredding encrypts the sidecar only; the live table stays plaintext** (§13.3). Encrypting the live table puts DataGit back on the `main` read path.
15. **Commit authors come from the authenticated principal, never from the client** (§15.2).
16. **Data merges into `main` apply immediately; schema merges produce a migration plan that is applied deliberately** (§10.4). Instant schema merges would break direct readers with no rollout window.
17. **Every feature ships on both PostgreSQL and MySQL.** The integration suite is ONE suite run against each engine, so there is no test only one engine runs. Genuine engine differences belong in the §4.3 capability matrix; a performance gap is not a capability difference and is measured and published in `docs/measurements.md`, never hidden there. PostgreSQL runs the same resumable migration state machine as MySQL despite having transactional DDL, so failure behaviour is identical and only tested once.
18. **Store SQL is written in ONE dialect and translated mechanically at the connection boundary** (`internal/db`). Mechanical translation is only valid for SPELLING differences — placeholders, identifier quoting. Where the engines differ SEMANTICALLY — upsert, generated-key retrieval, introspection, the write guard, column DDL — there is nothing to translate, and those go through explicit adapter methods carrying the reason they could not be shared. Never branch a query on the engine inline.
19. **A value's kind comes from the column's DECLARED kind, not the driver's Go type** (`fromDriver`). MySQL returns `[]byte` for DECIMAL, VARCHAR, and TEXT alike, so the bytes do not say what the value is; reading a DECIMAL as opaque bytes puts the wrong tag in the canonical encoding and changes a commit hash for a value that did not change.
20. **An unreachable database named by `DATAGIT_TEST_DSN` FAILS rather than skipping.** A silent skip reports "ok" for a suite that ran nothing, which is how an engine stays broken while CI stays green.

## Correctness strategy

Primary evidence is **differential testing against an in-memory reference model** (`internal/model`), not example-based tests. Random operation sequences run against both the model and the real implementation; resolved state, merge results, *and* conflict sets must match. PLAN.md §Verification lists the twelve standing invariants the harness asserts.

Extend the model **before** the implementation. If the model cannot express a feature, the feature is not specified well enough to build.

## Commands

```bash
make db-up              # PostgreSQL 16/17 and MySQL 8.4
make test               # unit, race detector on
make test-property      # the differential harness
make test-integration   # the SAME suite against PG 16, PG 17, and MySQL 8.4
make test-bench         # performance gates, all engines; fails a blown budget
make test-acceptance    # the README tour, verbatim
make test-frozen        # the frozen encoding and commit hash
make lint               # vet plus the internal/model import rule
make test-sdk-py        # Python SDK
make test-sdk-ts        # TypeScript SDK
make spike-s1 s3 s4 s5  # Phase 0 spikes, reproducible
```

## Correctness evidence so far

The differential harness has run 10.2M operations with zero divergence — after
finding five real bugs. 80 integration tests run against each of PostgreSQL 16,
PostgreSQL 17, and MySQL 8.4; six performance gates assert budgets on all three;
the parity gate compares both engines without a database; and the README tour
runs verbatim.

Running the suite on both engines is itself a bug-finding tool, not a checkbox.
It has already caught a `CAST(x AS CHAR)` that means `char(1)` on PostgreSQL and
unbounded text on MySQL, and a widening type change that looked incompatible
because one engine reports `numeric` where the other reports `decimal`.

## Conventions

- Section references in code comments and commit messages use the `§N.N` form and point at DESIGN.md.
- When implementation reveals that DESIGN.md is wrong, amend DESIGN.md in the same change. The design is the source of truth only if it stays true.
- DESIGN.md §14.1 carries targets; `docs/measurements.md` carries measurements. Where they disagree, the measurement wins and §14.1 should be corrected.
- New tests go in `test/integration` unless they need no database. Anything there runs on all three engines automatically, which is the point.
- A performance gap is not a capability difference and must not be recorded in the §4.3 matrix.
- DESIGN.md §20.1 lists seven open questions; PLAN.md maps each to the milestone that must close it. Do not drift past those boundaries silently.
