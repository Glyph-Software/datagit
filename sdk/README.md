# SDKs

Both SDKs are **generated stubs plus a thin ergonomic layer**. The generated
part is complete and unpleasant; the layer makes the common paths short without
hiding what the service does.

| | Path | Regenerate | Test |
|---|---|---|---|
| Python | `sdk/python` | `make sdk-py` | `make test-sdk-py` |
| TypeScript | `sdk/typescript` | `make sdk-ts` | `make test-sdk-ts` |

## Three things neither SDK does

**Neither lets you set a commit author.** The author comes from the credential
(DESIGN.md §15.2). An audit trail whose author is client-supplied is decoration,
so there is no field to pass — not a validated one, not one at all.

**Neither carries exact numbers as a float.** A value is hashed into history, so
a rounding difference on the way through the SDK would change the commit id for
data nobody edited. Python uses `Decimal`; TypeScript uses `bigint` and a
`Decimal` wrapper, because JavaScript's `number` is a float64 and cannot hold
either an exact decimal or every int64.

**Neither builds filters from strings.** Filters are typed expression trees with
no SQL text form, so there is nothing to inject into (§15.4). A hostile string
passed as a filter value stays a value.

## Rows are keyed by column id

Both SDKs key rows by **stable column id**, not by name (§10.5 rule 1). A rename
is metadata-only in DataGit, and keying by name would make a rename silently
change what a row means on the wire.

## Batching is not just ergonomics

A commit takes the branch's ref lock, so throughput is commits per second
regardless of how many rows each one carries (§11.3). One commit of a thousand
rows costs roughly what one commit of one row costs. Both SDKs expose a
transaction that buffers changes into a single commit; a loop of single-row
commits is the slowest way to write.

## Python

```python
from decimal import Decimal
from datagit import Client, col

with Client("datagit.internal:443", api_key=KEY) as c:
    items = c.repo("catalog").table("products")

    # Reading a BRANCH. Reads on main need no DataGit at all — query the table.
    for row in items.read(branch="q4-pricing", where=col(3) == "outdoor"):
        print(row)

    # One commit, however many rows.
    with items.transaction(branch="q4-pricing", message="Q4 pricing") as tx:
        tx.update(pk, {1: "TENT-4P", 4: Decimal("268.92")})
```

## TypeScript

```ts
import { DataGitClient, col, dec } from "@glyph-software/datagit";

const c = new DataGitClient({ baseUrl: "https://datagit.internal", apiKey: KEY });

for await (const row of c.scan({
  repo: "catalog", table: "products", branch: "q4-pricing",
  filter: col(3).eq("outdoor"),
})) {
  console.log(row);
}

await c.transaction(
  { repo: "catalog", table: "products", branch: "q4-pricing", message: "Q4 pricing" },
  (tx) => tx.update(pk, { 1: "TENT-4P", 4: dec("268.92") }),
);
```
