# Phase 0 spikes

**Throwaway code.** These programs exist to answer one question each and then be
deleted. They are not part of the shipped tree, are not held to its standards,
and nothing in `internal/` or `cmd/` may import them.

The one exception is S2, which was never a spike in this directory: its output is
the permanent reference model (`internal/model`) and differential harness
(`test/property`). PLAN.md says so explicitly.

Results and the design changes they forced are in
[docs/phase0/findings.md](../docs/phase0/findings.md).

| Spike | Question | Gates |
|---|---|---|
| **S1** `s1_resolution` | Does two-pass branch resolution (§7.3) hold up at 50M versions, and do the two resolution hazards actually bite? | M1 |
| **S3** `s3_commit` | Does the single-transaction commit path (§6.1) meet its latency target, and what does the ref lock cost under concurrency? | M1 |
| **S5** `s5_storage` | Is the storage estimate real, and does partition-drop pruning beat `DELETE`? | M1 |
| **S4** — not written | Does the journalled migration state machine survive crashes without transactional DDL (§10.4)? | M6, not M1 |

S4 is deliberately absent. MySQL is not on the v1.0 path, and spiking it now
would de-risk something two releases away.

## Running them

```bash
make db-up
make spike-s1     # generates ~15 GB; takes a few minutes
make spike-s3
make spike-s5
```

Each spike takes `-dsn` and can be pointed at either engine. They create and drop
their own schemas (`s3`, `s5`, `s5p`) and the top-level `datagit_v_products`
table; `make db-reset` clears everything.
