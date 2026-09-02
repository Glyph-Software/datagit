"""DataGit's Python SDK.

The generated gRPC stubs are complete and unpleasant. This layer exists to make
the common paths short without hiding what the service actually does.

Three things it does NOT do, deliberately:

It does not let you set a commit author. The author comes from the credential
(DESIGN.md §15.2); an audit trail whose author is client-supplied is decoration.
There is no field to pass.

It does not carry decimals as floats. A value is hashed into history, and a
rounding difference would change the commit id, so exact numerics travel as
`decimal.Decimal` and are put on the wire as strings.

It does not build predicates from strings. Filters are typed expressions with no
SQL text form, so there is nothing to inject into (§15.4).
"""

from .client import (
    Client,
    Repo,
    Table,
    Transaction,
    Conflict,
    DataGitError,
    ConflictError,
    NeedsMigrationError,
    col,
)

__all__ = [
    "Client",
    "Repo",
    "Table",
    "Transaction",
    "Conflict",
    "DataGitError",
    "ConflictError",
    "NeedsMigrationError",
    "col",
]
