# Contributing to DataGit

DataGit stores other people's production data and claims their history is
provable. That claim is only worth something if changes are held to it, so this
file is mostly about **what will get a change rejected** rather than about
formatting.

Read [CLAUDE.md](CLAUDE.md) first. It is written for AI assistants but it is the
shortest accurate description of the invariants, and every one of them applies
to human contributors identically.

---

## Before you write code

Three documents, in this order:

1. **[DESIGN.md](DESIGN.md) §5–§10** — storage layout, read and write paths, the
   merge algorithm, schema evolution. Implementation that disagrees with these is
   a bug in one of them, and which one is a conversation worth having before the
   code exists.
2. **[DESIGN.md](DESIGN.md) §19** — what was already considered and rejected, and
   why. Several appealing ideas are in there with the reason they do not work.
   Proposing one again is fine; proposing it without engaging with §19 is not.
3. **[docs/phase0/findings.md](docs/phase0/findings.md)** — eleven findings, five
   of them correctness bugs that were found the hard way. The code carries
   `finding Fn` markers at the 32 places they apply. **If you are touching
   resolution, merge, or the sidecar, read these first.**

---

## Setup

```bash
make db-up      # PostgreSQL 16, PostgreSQL 17, MySQL 8.4 in Docker
make build      # bin/datagit and bin/datagitd
```

Go 1.25 or later. The databases are needed for most of the test suites; see
[What CI does not run](#what-ci-does-not-run) below.

---

## The loop

```bash
make lint               # go vet plus the internal/model import rule
make test               # unit, race detector on
make test-property      # the differential harness
make test-integration   # the SAME suite against PG 16, PG 17, and MySQL 8.4
make test-bench         # performance gates; fails a blown budget
make test-acceptance    # the README tour, run verbatim
```

`make help` lists everything.

Run `make test-integration` before opening a pull request. It is the suite most
likely to catch a change that works on the engine you happened to be using.

---

## Rules that will get a change rejected

These are not style preferences. Each one is load-bearing, and most were learned
by getting them wrong.

### 1. Never put DataGit on the `main` read path

The live table must stay a clean, schema-unmodified materialization of
`main@HEAD`, because application readers query it directly with no DataGit hop.

That means **no added columns, no triggers, no view substitution on a tracked
live table on the happy path**. The one exception is the opt-in §6.3 write guard,
which is off by default for exactly this reason.

This constraint is why the storage design looks the way it does. A change that
relaxes it is not a refactor, it is a different product.

### 2. Every feature ships on both engines

`test/integration` is **one suite run against three engines**, not three suites.
A new test goes there and automatically runs on PostgreSQL 16, PostgreSQL 17, and
MySQL 8.4. That is the point: there is no test that only one engine runs, so a
feature cannot ship working on only one.

Write the test engine-neutrally. When a genuine difference forces a branch, the
fixture already has the seams — `f.dialect`, `f.currentSchema()`, `f.asText()` —
and adding a fourth is fine when the difference is real.

**A performance gap is not a capability difference.** It gets measured and
published in [docs/measurements.md](docs/measurements.md), never recorded in the
§4.3 capability matrix and never quietly omitted.

### 3. Store SQL is one dialect, translated mechanically

Store code writes PostgreSQL-flavoured SQL — `$N` placeholders,
`"double-quoted"` identifiers — and `internal/db` translates it for MySQL at the
connection boundary.

Mechanical translation is only valid for **spelling** differences. Where the
engines differ **semantically** — upsert, generated-key retrieval, introspection,
the write guard, column DDL — there is nothing to translate, and those go through
an explicit adapter method carrying the reason it could not be shared.

**Never branch a query on the engine inline.** Two versions of a statement means
two things that can be wrong and half the chance either is exercised.

### 4. The canonical encoding and commit hash are frozen

`datagit.commit.v1` (§12.1). Changing the value encoding or `commit_id`
construction invalidates **every commit hash ever written**, everywhere, with no
migration path. Golden tests guard it, and CI runs them as a separate job so the
failure is unmistakable.

If you believe a change here is necessary, that is a versioning discussion before
it is a code change.

### 5. A value's kind comes from the column's declared kind

Not from the driver's Go type. MySQL returns `[]byte` for DECIMAL, VARCHAR, and
TEXT alike, so the bytes do not say what the value is. Reading a DECIMAL as
opaque bytes puts the wrong tag in the canonical encoding and changes a commit
hash for a value that did not change.

### 6. `internal/model` is test-only, forever

The reference implementation must never be imported by production code. A
reference model that shares code with the implementation tests nothing. `make
lint` enforces this and will fail the build.

### 7. Refusals must explain themselves

DataGit refuses things on purpose: a table with no primary key, a merge over the
atomic apply limit, multiple merge bases, a protected branch with no approvals, a
narrowing migration without confirmation.

Every refusal names **what was refused, why, and what to do instead**, and
carries a `§N.N` reference where one applies. A generic error in place of one of
these is a regression even though nothing breaks.

### 8. Commit authors come from the authenticated principal

Never from the client. `CommitRequest` has no author field on any surface — gRPC,
REST, or SDK — and it never will. An audit trail whose author is client-supplied
is decoration.

---

## Adding a test

**Default to `test/integration`.** Anything there runs on all three engines
automatically, which is the whole reason it exists.

| Put it in | When |
|---|---|
| `test/integration` | it needs a database — the default |
| `internal/<pkg>` | it needs no database (encoding, planning, query construction) |
| `test/property` | it is an invariant that should hold over *random* operation sequences |
| `test/bench` | it asserts a performance **budget**, not a measurement |
| `test/acceptance` | it is a step in the README, run verbatim |

### Name the test after the property, not the function

The suite reads as a list of claims about the system. `TestMergeDeleteModify
AlwaysConflicts` says what must be true; `TestMerge3` says nothing.

### Say why the test exists

A failure message should tell someone who has never seen the code what broke and
why it matters. Compare:

```go
t.Errorf("got %d, want 2", n)
```

with what is actually in the suite:

```go
t.Errorf("the schema merge changed the live table immediately: %d columns, was %d. "+
    "Direct readers get no rollout window that way (§10.4)", after, before)
```

### Extend the reference model before the implementation

For anything the differential harness covers: if the model cannot express a
feature, the feature is not specified well enough to build yet.

---

## Changing an SDK or the proto

Two things are easy to forget and both are caught by CI.

**Regenerate the stubs.** They are committed, so they can silently fall behind
the proto — and an SDK whose stubs disagree with the service fails at a
customer's runtime rather than in your build.

```bash
make check-sdk-stubs    # regenerates and diffs; run it before you push
```

**Record a changeset**, so the change is released rather than sitting on `main`:

```bash
make changeset
```

The two SDKs share one version and release together; `sdk/README.md` explains
why, and what to do if a release half-fails. A change to the canonical encoding
is always a **major**, even when the SDK API is untouched.

---

## Adding an engine

The adapter boundary is `internal/adapter`. A new engine implements
`adapter.Adapter`, gets a pool under `internal/`, and is wired into
`internal/connect`. Then the existing integration suite runs against it unchanged
— which is the test of whether the boundary is in the right place.

Declare genuine differences in `Caps` and the §4.3 matrix. Do not paper over one:
if an engine cannot do something, the design accommodates the weaker engine
rather than pretending.

---

## Changing the design

**When implementation reveals that DESIGN.md is wrong, amend DESIGN.md in the
same change.** The design is the source of truth only as long as it stays true,
and a document that has quietly drifted from the code is worse than no document.

Where §14.1's targets and `docs/measurements.md` disagree, **the measurement
wins** and §14.1 should be corrected.

---

## Commit messages

Look at `git log` before writing one. The convention is:

- **A subject line that says what changed**, plus the milestone when it maps to
  one: `M6: schema changes flow branch -> proposal -> plan -> apply -> live table`
- **A body that explains why**, not what. The diff shows what. The body is for
  the constraint that forced the design, the alternative that was rejected, or
  the bug the change prevents.
- **`§N.N` references** pointing into DESIGN.md.
- **Bugs found are stated, not buried.** Several commits here say plainly that a
  test caught a real bug. That history is useful.

If a change was harder than it looks, the message is where to say why.

---

## What CI does not run

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs build, lint, unit
tests with the race detector, the differential harness, and the frozen-encoding
gate.

It does **not** run the database-backed suites, because it has no database
services configured:

- `make test-integration` — the 80-test suite, on all three engines
- `make test-bench` — the performance gates
- `make test-acceptance` — the README tour

**Run these locally before opening a pull request.** A green CI badge on this
repository does not currently mean the engines were exercised, and that gap is
stated here rather than left to be discovered.

The SDKs *are* covered, by [.github/workflows/sdk.yml](.github/workflows/sdk.yml):
both test suites, the stub-drift check, and an assertion that the two SDK
versions have not drifted apart. Releases run from
[.github/workflows/sdk-release.yml](.github/workflows/sdk-release.yml).

---

## Reporting a correctness bug

If you believe DataGit resolved, merged, or hashed something incorrectly, that
outranks everything else in this file. Include:

- the operation sequence, if you have it — the property harness prints a
  reproducible seed on failure
- which engine and version
- what you expected and what you got

A resolution or merge bug is a data-integrity bug. Say so in the title.

---

## License

Apache 2.0. By contributing you agree your contributions are licensed under it.
