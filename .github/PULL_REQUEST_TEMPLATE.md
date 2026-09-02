<!--
One template for both kinds of change. Delete the section that does not apply.

Read CONTRIBUTING.md if you have not. It is mostly about what will get a change
rejected, which is more useful to you before review than after.
-->

## What this changes, and why

<!--
The diff shows what. This is for why: the constraint that forced the design, the
alternative you rejected, or the bug this prevents. Reference DESIGN.md as §N.N.

If the change was harder than it looks, say why here.
-->

Fixes #

## Kind of change

- [ ] Bug fix
- [ ] Correctness fix — resolution, merge, or hash produced a wrong answer
- [ ] Feature
- [ ] Performance
- [ ] Docs, tests, or tooling only

### If this is a fix

<!-- What was wrong, and what now stops it coming back. -->

- **The bug:**
- **The test that fails without this change:**

### If this is a feature

<!-- Delete if not applicable. -->

- **§19 (already considered and rejected):** <!-- Does §19 cover this? If so, what is different? -->
- **Reference model extended first:** <!-- For anything the differential harness covers, the model comes before the implementation. If the model could not express it, the feature was not specified well enough to build. -->

## Invariants

Tick the ones this change comes near, and say below how it stays inside them.

- [ ] Touches a **tracked live table** — still no added columns, triggers, or view substitution on the happy path (§5.1)
- [ ] Touches **resolution** — `op <> 3` still filtered *outside* the union arms, no value filter pushed in; primary key is the only safe pushdown (§7.3, findings F1/F6)
- [ ] Touches **merge** — `changed_cols` still treated as a superset, decisions still compare values; "changes since base" still diffed in both directions (§9.2, findings F2/F4/F5)
- [ ] Touches the **canonical encoding or `commit_id`** — `datagit.commit.v1` is frozen; this is a versioning discussion before it is a code change (§12.1)
- [ ] Touches the **sidecar** — columns still named by stable column id, still append-only (§10.5)
- [ ] Adds or changes a **refusal** — it names what was refused, why, what to do instead, and carries a §N.N reference
- [ ] Touches **store SQL** — one dialect, translated mechanically at the `internal/db` boundary; semantic differences go through an explicit adapter method, never an inline engine branch
- [ ] None of the above

<!-- How it stays inside them: -->

## Tests

**CI does not run the database-backed suites** — it has no database services configured.
Run them locally and paste or summarise the result.

- [ ] `make lint`
- [ ] `make test`
- [ ] `make test-property`
- [ ] `make test-integration` — **PostgreSQL 16, PostgreSQL 17, and MySQL 8.4**, not just the one you develop against
- [ ] `make test-bench` — if this could move a performance budget
- [ ] `make test-acceptance` — if this changes anything the README tour walks through
- [ ] New tests are in `test/integration` unless they need no database
- [ ] New tests are named after the property they assert, and their failure messages say why the property matters

<!-- Results: -->

## Both engines

- [ ] Ships on PostgreSQL and MySQL, with no test that only one engine runs
- [ ] Any genuine engine difference is declared in `Caps` and the §4.3 capability matrix
- [ ] Any performance gap is measured in `docs/measurements.md` — not recorded as a capability difference, not omitted

## Documentation

- [ ] DESIGN.md amended in this change, if implementation revealed it was wrong
- [ ] PLAN.md milestone status updated, if this closes or moves one
- [ ] `docs/measurements.md` updated, if a number changed — where §14.1 and a measurement disagree, the measurement wins

## SDKs and proto

Skip if this touches neither.

- [ ] `make check-sdk-stubs` passes — regenerated stubs are committed
- [ ] `make changeset` recorded, so the change is released rather than sitting on `main`
- [ ] A change to the canonical encoding is a **major**, even when the SDK API is untouched
