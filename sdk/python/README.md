# datagit

Python SDK for [DataGit](https://github.com/Glyph-Software/datagit) — Git-style
version control for rows in your own PostgreSQL or MySQL database.

```bash
pip install datagit
```

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

## Three things this SDK will not let you do

**Set a commit author.** It comes from the credential. An audit trail whose author
is client-supplied is decoration, so there is no field to pass.

**Send a decimal as a `float`.** Values are hashed into history, so a rounding
difference would change a commit id for data nobody edited. Exact numbers travel
as `decimal.Decimal` and go on the wire as strings.

**Build a filter from a string.** Filters are typed expression trees with no SQL
text form, so there is nothing to inject into. A hostile string passed as a filter
value stays a value.

## Rows are keyed by column id

Not by name. A rename is metadata-only in DataGit, and keying by name would let a
rename silently change what a row means on the wire.

## Batch your writes

A commit takes the branch's ref lock, so throughput is commits per second
regardless of how many rows each carries. One commit of a thousand rows costs
roughly what one commit of one row costs, and a loop of single-row commits is the
slowest possible way to write.

## Versioning

This package and the TypeScript `@glyphsoftware/datagit-sdk` package are the same
contract twice and release on one version number. A change to the canonical
encoding is always a major, even when this SDK's own API is untouched.

Apache 2.0.
