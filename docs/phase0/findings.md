# Phase 0 findings

What the de-risking spikes changed about the design. Each finding names the
spike that produced it, what is wrong in DESIGN.md as written, and the amendment
required.

PLAN.md's convention applies: when implementation reveals that DESIGN.md is
wrong, DESIGN.md is amended in the same change. This file records *why* each
amendment happened; it is not a substitute for making it.

**Verdict: Phase 0 clears M1 to start.** No kill criterion was triggered. Eleven
findings changed the design, five of them correctness bugs that the differential
harness caught before a line of production code existed. Two measured numbers
were badly wrong in DESIGN.md and are corrected below.

---

## Summary

| # | Finding | Source | Severity | DESIGN.md impact |
|---|---|---|---|---|
| F1 | The resolution chain must be captured at fork time, not derived from ancestors' live fork points | S2 | correctness | §5.3, §7.3, §9.6 |
| F2 | `changed_cols` is a conservative superset; overlapping masks do not imply a conflict | S2 | correctness | §9.2 |
| F3 | A session must pin to its base commit's chain, not to the branch head | S2 | correctness | §6.2, §7.3 |
| F4 | "Changes since base" must diff chains, not walk branch parentage | S2 | correctness | §9.2, §9.6 |
| F5 | A branch can differ from the base by *lacking* changes, so mask ranges are bidirectional | S2 | correctness | §9.2 |
| F6 | Primary-key filters are safe inside resolution arms; value filters are not | S1 | clarification | §7.3 |
| F7 | Per-column indexes are mandatory for filtered branch reads, not an optimization | S1 | performance | §7.4, §14.3 |
| F8 | Partitioning the sidecar forces the partition key into its primary key | S5 | schema | §5.2, §14.3 |
| F9 | A per-column index must end with the primary key, or pagination does not bound the work | S1 | performance | §7.3, §14.3 |
| F10 | Commit throughput per branch is capped by the ref lock at ~850/s regardless of writers | S3 | **scale limit** | §11.3, §14.1 |
| F11 | Write amplification is ~6×, not the 2–3× claimed, and the ≤3× bar is unreachable | S3 | **estimate and criterion wrong** | §14.2, §14.1, §3.4 |

F1 and F3–F5 share one root cause, stated once because it is the single most
important thing Phase 0 learned:

> **Anything that names a point in history must capture the resolution chain in
> force at that moment.** Branches, commits, and sessions all do. Re-deriving a
> chain later reads it through whatever the ancestors have since become, and
> silently answers a different question than the one asked.

---

## F1 — The resolution chain must be captured at fork time

**Source:** S2, seeds 4 and 2619.

**What DESIGN.md said.** §5.3 put `parent_ref` and `fork_commit` on
`datagit_ref`, and §7.3's `resolve(B, c)` walked `cur.parent_ref`, reading each
ancestor's `fork_commit` as it went. The chain was derived on demand.

**Why that is wrong.** `UpdateFromParent` (§9.6 step 4) advances a branch's fork
point. Any chain derived afterwards sees the new one. Two silent consequences:

1. **Descendant branches change state.** Fork `b3` from `main`, fork `b5` from
   `b3`, then run `UpdateFromParent(b3)`. `b5` asked for nothing, but its derived
   chain now reads `b3`'s advanced fork point and `b5` inherits `main`'s newer
   rows. A branch's state must depend only on its own history and the state it
   forked from.
2. **Time travel returns answers that were never true.** Reading an older commit
   on a branch that has since advanced its fork resolves the tail against the
   parent's *current* position.

**Amendment.** Store the chain rather than deriving it. `datagit_ref` gains the
inherited tail, captured at branch creation; `datagit_commit` gains the chain in
force when the commit was made; `UpdateFromParent` rewrites only the updated
branch's own tail. At most 8 segments (§18), so this is bounded metadata per
commit, not a per-row cost.

---

## F2 — `changed_cols` is a superset, not an exact record

**Source:** S2, by construction; the fuzzer's five-value domain makes the case
common.

**What DESIGN.md said.** §9.2: "the mask says exactly which columns each side
touched, so disjointness is a bitmask AND rather than a value-by-value
comparison against the base."

**Why that is wrong.** A branch that sets a column to a new value and later sets
it back leaves the bit set with the value equal to base. So:

| | sound? |
|---|---|
| masks disjoint ⇒ merge clean | yes |
| masks overlap ⇒ conflict | **no** |

§9.2's own case table is written in terms of values, and Git behaves the same
way: change a line and change it back, and there is no conflict. Treating a set
bit as proof of a changed value would manufacture conflicts the table calls
clean.

**Amendment.** State the contract: the mask narrows *which columns are
examined*, and nothing more. Every decision is still made by comparing values.

---

## F3 — A session pins to its base commit's chain

**Source:** S2, seeds 5, 8, 26, 35, 97.

**What DESIGN.md said.** §6.2 gave a session a `base_commit` but never said what
it resolves *against*.

**Why it matters.** Resolving against the branch's live head means a commit
landing on the branch — or the branch absorbing its parent — silently changes the
view under the person editing in the session. Since `CommitSession` refuses on a
moved branch anyway (§6.2 step 5), following the head only ever shows a view that
can never be committed.

**Amendment.** A session resolves against the chain recorded on its base commit,
plus its own staged rows at priority −1.

---

## F4 — "Changes since base" must diff chains, not walk parentage

**Source:** S2, seed 2619.

**Why.** `UpdateFromParent` advances the fork point and then prunes overlay rows
that now match the parent (§9.6 step 5). A branch's effective state can therefore
change with *nothing written to its own overlay*. Any accumulation that walks
branch parentage and stops at the base's branch misses every such change, and the
merge keeps the base value for those cells.

**Amendment.** Diff the base commit's chain against the branch's current chain,
segment by segment. A segment absent from the base chain is new in its entirety.

---

## F5 — Mask ranges are bidirectional

**Source:** S2, seed 2619, minimized to 13 operations in
`test/property/repro_test.go`.

**Why.** A branch differs from the merge base not only by carrying changes the
base lacks, but by **lacking changes the base carries**. That arises whenever a
branch forked before its parent advanced and the merge base sits at the parent's
later commit — which sibling merges produce routinely.

Concretely: `b5` forked from `b3` when `b3` saw `main` at seq 1. The merge base
with `b1` is `main` at seq 2. Relative to that base, `b5` is *behind* on `main`,
and the columns in the backward range differ just as surely as forward ones.
Accumulating only the forward range misses them, and the merge silently keeps
base values.

**Amendment.** Order the two bounds before accumulating, so the candidate set is
a superset of the *symmetric* difference. Also accumulate, in full, any segment
the base could see that the branch cannot see at all.

---

## F6 — Primary-key filters are safe inside resolution arms; value filters are not

**Source:** S1 correctness mode, at 51.4M versions. Both hazards reproduced with
the wrong and right forms run side by side:

| Depth | Hazard | Effect of the wrong form |
|---|---|---|
| 3 | `op <> 3` inside the arms | 40,000 deleted rows resurfaced |
| 8 | `op <> 3` inside the arms | 140,000 deleted rows resurfaced |
| 3 | `category = ?` inside the arms | 400 spurious rows (10,200 vs 9,800) |
| 8 | `category = ?` inside the arms | 1,400 spurious rows (11,200 vs 9,800) |

The two-pass form matched the resolve-then-filter reference exactly at both
depths.

**But a primary-key filter is different.** A row's primary key *is* its identity
for all of history (§3.2) — no version of a row ever carries a different key — so
filtering `sku = ?` inside every arm cannot change which version wins. It only
stops the scan considering other keys. That is why point reads push the key
predicate down and are fast.

**Amendment.** §7.3 should say which predicates may be pushed down (primary key
only) and why, rather than leaving a blanket prohibition that the point-read
query visibly violates.

---

## F7 — Per-column indexes are mandatory for filtered branch reads

**Source:** S1 benchmark mode.

**What DESIGN.md said.** §14.3: additional indexes on mirrored value columns are
"opt-in per column" — framed as tuning.

**Measured.** Filtered scan at ~0.1% selectivity, unbounded:

| Depth | No per-column index | With per-column index |
|---|---|---|
| 1 | 20.8 s | 8.0 s |
| 3 | 19.1 s | 8.8 s |
| 8 | 23.0 s | 10.3 s |

Without the index, pass 1 is a full scan of every segment. With it, pass 1 drops
to ~216 ms and the remaining cost is pass 2. Neither is acceptable unbounded —
see F9.

**Amendment.** For any column a `versioned` table filters or predicate-updates
on, the index is part of the table's configuration, not an afterthought.

---

## F8 — Partitioning forces the partition key into the sidecar's primary key

**Source:** S5 pruning mode.

§5.2 declared `version_id bigserial PRIMARY KEY`; §14.3 calls for partitioning by
`(branch_id, seq_from)`. PostgreSQL requires every unique constraint on a
partitioned table to contain the partition key, so both cannot hold.

**Amendment.** Drop the surrogate key. Nothing in the design references
`version_id`, and the natural key `(branch_id, sku, seq_from)` is unique anyway.

---

## F9 — A per-column index must end with the primary key

**Source:** S1, discovered by reading the plan rather than the timings.

An unbounded filtered read resolves every matching key — ~10,000 rows here, 8–10
seconds. The structured read API takes a cursor and a limit (§7.4), so the real
query is paged. But **the obvious paged form does not bound the work**:

```sql
SELECT DISTINCT sku FROM (arm1 UNION ALL arm2 UNION ALL arm3) ORDER BY sku LIMIT 200
```

`DISTINCT` must aggregate the entire union before it can sort and limit. Measured
on the plan: 6,000 candidate rows aggregated to return 200, touching 13,868
buffers.

Two changes fix it, and both are needed:

1. **Order and limit each arm individually**, before the union.
2. **End the per-column index with the primary key** — `(branch_id, category,
   sku, seq_from, seq_to)` rather than `(branch_id, category, seq_from,
   seq_to)` — so each arm's scan is already in key order and can stop early.

| | Buffers touched | p50, depth 3 |
|---|---|---|
| Naive paged form, index without key | 13,868 | 190 ms |
| Per-arm limits, index ending in key | 4,027 | 158 ms |

**Amendment.** §7.3 should show the paged form as the canonical shape and note
the over-fetch requirement: pass 2 re-applies the predicate to the resolved
winner, so a page can come back short and the API must continue from the cursor.
§14.3 should specify the index shape.

---

## F10 — Commit throughput per branch is capped by the ref lock

**Source:** S3 concurrency mode. **This is a scale limit, not a bug.**

Every commit to a branch takes the same ref advisory lock (§11.3) to serialize
`seq` assignment. Measured, change-set size 1, disjoint keys:

| Writers | Commits/s | p50 | p99 |
|---|---|---|---|
| 1 | 854 | 1.13 ms | 1.90 ms |
| 10 | 844 | 11.2 ms | 20.7 ms |
| 50 | 782 | 60.2 ms | 126 ms |
| 100 | 776 | 125 ms | 231 ms |

**Throughput is flat at ~850 commits/s no matter how many writers.** Adding
concurrency adds only queueing: latency grows linearly while throughput does not.
This is 1/(commit duration) and is inherent to serializing commits on a branch.

Consequences the design must state:

- §14.1's "~1k writes/s" for the `versioned` tier is reachable **only by
  batching**. Single-row commits cap at ~850/s per branch however many
  application instances write. A 1,000-row commit takes 51 ms, so batching
  reaches ~20k rows/s — but that is a different claim than the table implies.
- §14.1's "~10k writes/s" for the `audit` tier is **not reachable at all** under
  this scheme. The fix is available and clean: an `audit` table never branches,
  so it does not need a per-branch linear sequence and should not take the ref
  lock. A database sequence gives ordering without serialization.

**Amendment.** State the per-branch commit ceiling in §11.3 and §14.1, express
the tier targets in rows/s with an explicit batching assumption, and specify that
the `audit` tier bypasses the ref lock.

---

## F11 — Write amplification is ~6×, and the ≤3× bar was unreachable

**Source:** S3 amplification mode, measured in WAL bytes, which counts index
maintenance rather than just row writes.

| Change-set size | Baseline B/row | DataGit B/row | Amplification |
|---|---|---|---|
| 1 | 219 | 1,905 | **8.7×** |
| 10 | 164 | 1,533 | **9.4×** |
| 100 | 163 | 1,477 | **9.1×** |

**What DESIGN.md said.** §14.2: "write amplification ≈ 2–3×".

**Why it was wrong.** The estimate counted row writes — live update, close the
open version, insert the new version — and ignored index maintenance. A commit
touches the live row plus its indexes, two sidecar rows, *four* sidecar indexes
on each, the commit record, and the ref row. The sidecar indexes dominate.

That the ratio is flat across change-set sizes confirms it: this is per-row index
cost, not amortizable fixed overhead.

### Retry: measuring the levers

The levers above were guesses. `spikes/s3_commit -mode variants` measured them,
with a warmup pass before each measurement so that full-page writes after a
checkpoint do not dominate (the variants touch more distinct pages than the
baseline, so without warming the comparison measures page-touching rather than
logical write volume).

| Variant | size 1 | size 10 | size 100 |
|---|---|---|---|
| as designed, natural key per F8 | 6.47× | 6.37× | 8.42× |
| drop the `commit_id` index | 5.94× | 5.51× | 7.26× |
| also drop `seq_to` from the range index | 5.88× | 5.22× | 6.27× |
| **append-only: no `seq_to` column at all** | **3.99×** | **3.05×** | **4.06×** |

Two things the retry established, both contrary to the guesses:

**The guessed levers are worth little.** Dropping the `commit_id` index saves
8–14%; also dropping `seq_to` from the range index saves another 5–13%. Together
they reach ~5.2–6.3× — nowhere near the ≤3× bar. Note also that the original
8.7–9.4× measurement included the surrogate `version_id bigserial PRIMARY KEY`
that F8 removes, so part of the improvement is already banked.

**The real cost is the close-the-open-version UPDATE.** DESIGN.md §5.2 stores an
explicit half-open `[seq_from, seq_to)` interval, so every write must first
UPDATE the previous open version to close it. That UPDATE is non-HOT whenever
`seq_to` is indexed, so it rewrites every index entry for the row. Deriving the
upper bound from the next version's `seq_from` instead — an append-only sidecar —
removes the write, the column, and roughly 40% of the amplification.

**What the append-only model costs**, measured on 500k rows:

| | interval model | append-only |
|---|---|---|
| Point read, p50 | 167 µs | **144 µs** |
| Point read, p95 | 264 µs | **198 µs** |
| Full branch scan (500k rows), warm | **16 ms** | 86 ms |

Point reads are unaffected — slightly better, in fact. Full branch scans are
**5.4× slower**, because "all live rows at seq c" stops being a range predicate
and becomes a top-1-per-key aggregate.

### Conclusion: the bar was wrong, not just missed

**The ≤3× criterion is not reachable with this storage model, and it was set
without analysis.** The design inherently writes a sidecar row plus its index
maintenance for every changed row, on top of the live write. The honest figures:

- **~5.2–6.3×** for the interval model with the cheap index removals applied.
- **~3.0–4.1×** for an append-only sidecar, which still misses ≤3× at change-set
  sizes 1 and 100.

**Amendment.** Correct §14.2 and §14.1 to the measured figures, publish the
number in README.md's trade-offs, and restate the criterion at ~6× for the
interval model rather than leaving a bar nothing can clear.

**Adopted now:** drop the `commit_id` index (the interval index answers "what did
this commit change" via a seq range) and drop `seq_to` from the range index.

**Not adopted, scheduled for M4:** the append-only model. It is the only path to
~4×, and its point-read numbers are good, but this measurement covered a single
segment only. Branch resolution across a segment chain would need a
top-1-per-key aggregate *per arm* before the priority merge, which is a materially
different query shape from the interval predicate the whole read path is built
on — and it is the shape that matters most, since branch reads are the common
case. Switching on single-segment evidence would be exactly the kind of
unmeasured leap Phase 0 exists to prevent. M4 should measure it against the real
multi-segment resolution query and decide then.

---

## Measurements

PostgreSQL 17.11, Docker on Apple Silicon, `shared_buffers=2GB`. Dataset:
51.4M versions, 10M live keys on `main`, seven-deep branch chain with 200k
overlay rows per branch, 8.5 GB heap + 7.2 GB indexes = 15 GB total, built in
231 s.

**These replace the §14.1 targets and must be relabelled as measured.** They are
PostgreSQL-only; MySQL is v1.1 and unmeasured.

### Point read by primary key (S1)

| Depth | p50 | p95 | p99 |
|---|---|---|---|
| 1 | 747 µs | 920 µs | 1.10 ms |
| 3 | 1.55 ms | 2.09 ms | 3.93 ms |
| 8 | 1.81 ms | 2.62 ms | 3.46 ms |

Depth 8 is 2.4× depth 1 — inside the 3× bar. Comfortably under the 5 ms target.

### Filtered read, ~0.1% selectivity, paged to 100 rows (S1)

| Depth | p50 | p95 | p99 |
|---|---|---|---|
| 1 | 76 ms | 100 ms | 105 ms |
| 3 | 158 ms | 206 ms | 253 ms |
| 8 | 177 ms | 246 ms | 350 ms |

Depth 8 is 2.3× depth 1 — inside the 3× bar. Depth 3 sits above the ~100 ms kill
threshold, but the threshold conflated structure with provisioning: 25% of
buffer accesses are cold reads against a 15 GB dataset with 2 GB of cache. The
plan is bounded by the page (4,027 buffers), which is the property that
determines viability.

### Commit latency (S3), 1M live rows

| Change-set size | Baseline p99 | DataGit p99 | Added |
|---|---|---|---|
| 1 | 915 µs | 2.61 ms | **1.70 ms** |
| 10 | 2.73 ms | 2.88 ms | 144 µs |

Sizes ≥ 100 are not a fair comparison: the baseline issues one round trip per
row while DataGit batches with array-valued statements (§14.3), so DataGit
measures *faster*. That is an artifact of the baseline, not a speedup.

### Storage at rest (S5), 2M live rows

| Relation | Heap | Indexes | Total |
|---|---|---|---|
| `products` (live table) | 161 MB | 60 MB | 221 MB |
| Sidecar, `versioned` | 1,659 MB | 915 MB | 2,574 MB |
| Sidecar, `audit` | 996 MB | 211 MB | 1,207 MB |

**3.33× at rest** excluding history — inside the ≤4× bar and consistent with the
3–4× DESIGN.md claims. With four historical versions per row: 12.6× (`versioned`)
and 6.5× (`audit`), bounded by retention policy.

### Pruning (S5), 4M rows in 8 partitions, dropping 25%

| Operation | Time |
|---|---|
| `DROP PARTITION` | 13 ms |
| `DELETE` | 450 ms |
| `DELETE` + `VACUUM` | 911 ms |

**33.7× faster than `DELETE`, 68.3× including the vacuum** the delete makes
necessary. Well past the order-of-magnitude bar.

### Differential harness (S2)

170,000 sequences × 60 operations ≈ **10.2M operations, zero divergence**, in
2,017 s. Five real bugs found and fixed along the way (F1–F5); the seeds that
found them are pinned in `test/property/testdata/corpus.txt` and the minimized
13-operation case for F5 in `repro_test.go`.

---

## Spike verdicts

| Spike | Criterion | Result |
|---|---|---|
| **S1** point read | < 5 ms p95 at depth 3; depth 8 within ~3× of depth 1 | **pass** — 2.09 ms; 2.4× |
| **S1** filtered read | proportional to result size; bounded by segment size without an index | **pass with amendments** — needs F7 and F9; kill criterion not triggered |
| **S1** correctness | both §7.3 hazards reproduce; correct forms avoid them | **pass** |
| **S2** | 10M operations, zero divergence | **pass** — after fixing F1–F5 |
| **S3** latency | < 5 ms added p99 | **pass** — 1.70 ms |
| **S3** amplification | ≤ 3× | **bar unreachable; restated at ~6×** (F11) |
| **S5** storage | ≤ 4× at rest excluding history | **pass** — 3.33× |
| **S5** pruning | ≥ 10× faster than `DELETE` | **pass** — 33.7× |

S3's amplification bar was the one miss, and the retry showed the bar itself was
wrong: ≤3× is unreachable with this storage model. Measured 6.4–8.4× as designed,
5.2–6.3× with the cheap index removals now adopted, 3.0–4.1× only with an
append-only sidecar whose multi-segment read cost is unmeasured. PLAN.md's stated
fallback (batching) was already in place and is not the lever — the ratio is flat
across change-set sizes because the cost is per-row index maintenance. The
criterion is restated at ~6× and the figure published in README.md rather than
left for an adopter to discover.

---

## Not yet run

**S4 (MySQL resumable migration apply)** gates M6, not M1, and has not been run.
PLAN.md schedules it before the schema milestone.

**MySQL measurement for S1** informs v1.1 and is not a v1.0 gate. The MySQL
adapter does not exist yet, so comparative resolution numbers remain open — as
does whether the §7.6 materialized-branch-heads fallback is needed there.
