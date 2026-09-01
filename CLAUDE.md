# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

**Specification only — no code exists yet.** The repository contains four documents and nothing else:

| File | Role |
|---|---|
| [README.md](README.md) | Project overview, core concepts, worked CLI/SDK examples, stated trade-offs, roadmap. |
| [DESIGN.md](DESIGN.md) | **Source of truth.** 20-section technical design with DDL, algorithms, and rejected alternatives. Section references (§7.3, §9.2) throughout the codebase and this file point here. |
| [PLAN.md](PLAN.md) | Build sequence: Phase 0 de-risking spikes → M0 scaffolding → M1–M4 (v0.1→v1.0, PostgreSQL only) → M5–M7 (v1.1 MySQL, v1.2 schema, v1.3 compliance). |
| CLAUDE.md | This file. |

Before writing implementation code, read DESIGN.md §5–§10. Before proposing an architectural change, read §19 — it records what was already considered and rejected, with reasons.

## What DataGit is

A stateless Go service between an application and its existing PostgreSQL or MySQL database, adding Git-style version control (commits, branches, cell-level three-way merge, time travel, blame) to selected tables **in place**. Rows stay in the user's database. DataGit owns writes, history, and branches — not reads on `main`. Curation (branch, review, merge for rows) is the wedge; audit-grade history is what makes it trustworthy (§1.1).

## Architecture in one pass

Three storage layers per tracked table (§5.1):

- **Live table** — the application's own table, schema unmodified. Invariant: it *is* `main@HEAD`.
- **Version sidecar** `datagit_v_<table>` — typed mirrored columns named by stable column id (`c_<id>`), one row per row-version over a half-open `seq` interval, tagged with the branch that produced it, a `changed_cols` bitmask over column ids, and a `session_id` while staged.
- **Control tables** `datagit_*` — repos, commits, refs, sessions, idempotency, proposals, conflicts, keys, journals.

Reads on `main` go straight to the live table with no DataGit hop (§7.1). A `main` commit is one RPC carrying its whole change set, applied in one transaction (§6.1). Branch reads resolve each primary key through a priority chain of `(branch, seq)` segments down to `main`, in two passes when filtered (§7.3). Diffs are interval range scans, so they cost the size of the change (§8.1). Merges are three-way against the LCA, resolved per cell via `changed_cols` (§9.2). A branch catches up with its parent via `UpdateFromParent`, which advances its fork point so the segment chain stays a tree (§9.6).

## Invariants that constrain code changes

These look like ordinary refactors and are not. Each is load-bearing.

1. **Never add columns, triggers, or view substitutions to a tracked live table on the happy path.** It must stay a clean, schema-unmodified materialization of `main@HEAD`, because application readers query it directly. This kills the "visibility columns" model — see §19.2.
2. **Never put DataGit on the `main` read path.** It is not a latency or availability dependency for production reads. This is the constraint the whole storage design exists to satisfy.
3. **There is no uncommitted state on `main`** (§6.1). No sidecar row on `main` ever carries the zero commit hash or a `session_id`. Sessions exist only on other branches. Anything that stages on `main` before committing reopens the audit hole the design closed.
4. **In resolution queries, filter `op <> 3` *outside* the union arms, and never push a user filter into the arms** (§7.3). Either mistake lets an inherited parent row resurface after the branch deleted or changed it. Filtered reads use the two-pass form: candidate keys from any segment, then full resolution of exactly those keys, then the filter. These are the two most likely correctness bugs in the system and both are harness invariants.
5. **Canonical value encoding and `commit_id` construction are frozen** as `datagit.commit.v1` (§12.1). Changing them after any history exists invalidates every commit hash ever written. Guarded by golden tests.
6. **Sidecar columns are named by stable column id from the very first sidecar** (§10.5). Retrofitting ids later is a sidecar rewrite. Sidecar columns are append-only; narrowing type changes fork to a new id rather than coercing history.
7. **`internal/model` (the reference implementation) must never be imported by non-test code.** A reference model that shares code with the implementation tests nothing.
8. **Delete/modify is always a conflict** (§9.2). Multiple merge bases are **refused with candidates named**, never silently resolved (§9.1). Rebase is not offered; it rewrites hashes.
9. **Merges above the atomic apply limit are refused unless the caller opts into chunked apply**, which flags the ref `merge_in_progress` for the duration (§9.5). Never silently chunk.
10. **Purged commits are marked `integrity = 'purged'`, never re-hashed** (§13.4). Hiding the gap makes the audit trail lie.
11. **Crypto-shredding encrypts the sidecar only; the live table stays plaintext** (§13.3). Encrypting the live table puts DataGit back on the `main` read path.
12. **Commit authors come from the authenticated principal, never from the client** (§15.2).
13. **Data merges into `main` apply immediately; schema merges produce a migration plan that is applied deliberately** (§10.4). Instant schema merges would break direct readers with no rollout window.
14. **Every feature ships on both PostgreSQL and MySQL by v1.1; v1.0 is PostgreSQL only.** Genuine engine differences belong in the §4.3 capability matrix; a performance gap is not a capability difference and is measured and published, never hidden there. PostgreSQL runs the same resumable migration state machine as MySQL despite having transactional DDL, so failure behaviour is identical and only tested once.

## Correctness strategy

Primary evidence is **differential testing against an in-memory reference model** (`internal/model`), not example-based tests. Random operation sequences run against both the model and the real implementation; resolved state, merge results, *and* conflict sets must match. PLAN.md §Verification lists the twelve standing invariants the harness asserts.

Extend the model **before** the implementation. If the model cannot express a feature, the feature is not specified well enough to build.

## Commands

None exist yet. PLAN.md §M0.2 and §Verification specify the intended Makefile targets — `test`, `test-integration`, `test-property`, `test-crash`, `test-acceptance`, `verify-parity`, `bench`, `lint`, `proto`. Create them in M0; do not invent alternatives.

## Toolchain

Present: Go 1.25.1, Docker 29.4.3, protoc 34.0.
Missing, needed for M0: `buf` and the `psql` client. The `mysql` client is needed from M5.

Target engines: PostgreSQL 16 and 17 for v1.0; MySQL 8.4 from v1.1 (§M0.2, §M5).

## Conventions

- Section references in code comments and commit messages use the `§N.N` form and point at DESIGN.md.
- When implementation reveals that DESIGN.md is wrong, amend DESIGN.md in the same change. The design is the source of truth only if it stays true.
- DESIGN.md §14.1 numbers are **targets**, not measurements, and are for PostgreSQL. Relabel them explicitly once Phase 0 produces real figures; MySQL targets come from measurement in M5, never by assumption.
- DESIGN.md §20.1 lists seven open questions; PLAN.md maps each to the milestone that must close it. Do not drift past those boundaries silently.
