"""The ergonomic client."""

from __future__ import annotations

import contextlib
from dataclasses import dataclass, field
from typing import Any, Iterator, Mapping, Sequence

import grpc

from .v1 import datagit_pb2 as pb
from .v1 import datagit_pb2_grpc as rpc
from .values import from_wire, to_wire


class DataGitError(Exception):
    """A refusal from the service, carrying the reason it gave.

    DataGit's refusals are deliberate and explain themselves -- a table with no
    primary key, a merge over the atomic apply limit, a protected branch with no
    approvals. The message is the useful part, so it is preserved rather than
    replaced with a status name.
    """

    def __init__(self, err: grpc.RpcError):
        self.code = err.code()
        super().__init__(err.details() or str(err))


class ConflictError(DataGitError):
    """A merge that did not apply because cells disagree.

    Not an error in the sense of something going wrong: DataGit surfaces
    conflicts rather than guessing, and nothing was applied. The conflicts are on
    `.conflicts`.
    """

    def __init__(self, conflicts: Sequence["Conflict"]):
        self.conflicts = list(conflicts)
        Exception.__init__(
            self,
            f"{len(self.conflicts)} conflict(s); nothing was applied. "
            f"Resolve them and merge again",
        )


class NeedsMigrationError(Exception):
    """A merge whose data applied but whose SHAPE change is waiting.

    Not a failure. The data half is committed; the schema change is a migration
    plan to be applied deliberately, because applications read the live table
    directly and a column that appears or vanishes mid-query has no rollout
    window (§10.4).
    """

    def __init__(self, plan_id: int, ops: Sequence[str]):
        self.plan_id = plan_id
        self.ops = list(ops)
        super().__init__(
            f"data merged; migration plan {plan_id} is pending with "
            f"{len(self.ops)} operation(s). Apply it when readers can tolerate it"
        )


@dataclass(frozen=True)
class Conflict:
    pk: bytes
    column: str
    kind: str
    base: Any
    ours: Any
    theirs: Any


@dataclass(frozen=True)
class Column:
    """A column reference, for building typed filters."""

    id: int

    def __eq__(self, other: Any) -> pb.Expr:  # type: ignore[override]
        return _cmp(self.id, pb.COMPARE_OP_EQ, other)

    def __ne__(self, other: Any) -> pb.Expr:  # type: ignore[override]
        return _cmp(self.id, pb.COMPARE_OP_NE, other)

    def __lt__(self, other: Any) -> pb.Expr:
        return _cmp(self.id, pb.COMPARE_OP_LT, other)

    def __le__(self, other: Any) -> pb.Expr:
        return _cmp(self.id, pb.COMPARE_OP_LE, other)

    def __gt__(self, other: Any) -> pb.Expr:
        return _cmp(self.id, pb.COMPARE_OP_GT, other)

    def __ge__(self, other: Any) -> pb.Expr:
        return _cmp(self.id, pb.COMPARE_OP_GE, other)


def col(column_id: int) -> Column:
    """Reference a column by its STABLE id, so a rename does not break a filter.

    Filters build a typed expression tree. There is no string form and therefore
    nothing to inject into (§15.4).
    """
    return Column(column_id)


def _cmp(column_id: int, op: int, value: Any) -> pb.Expr:
    return pb.Expr(
        compare=pb.Compare(col=column_id, op=op, value=to_wire(value))
    )


def and_(*terms: pb.Expr) -> pb.Expr:
    return pb.Expr(and_=pb.And(terms=list(terms)))


def or_(*terms: pb.Expr) -> pb.Expr:
    return pb.Expr(or_=pb.Or(terms=list(terms)))


class Client:
    """A connection to a DataGit service.

    The API key identifies the principal, and the principal is what commits are
    attributed to. There is no way to commit as someone else, by design.
    """

    def __init__(self, target: str, api_key: str, *, secure: bool = True):
        creds = grpc.ssl_channel_credentials() if secure else None
        if secure:
            self._channel = grpc.secure_channel(target, creds)
        else:
            # Plaintext is for local development. Over a network it sends the API
            # key in the clear.
            self._channel = grpc.insecure_channel(target)
        self._meta = (("authorization", f"Bearer {api_key}"),)
        self.repository = rpc.RepositoryStub(self._channel)
        self.data = rpc.DataStub(self._channel)
        self.version = rpc.VersionStub(self._channel)
        self.branching = rpc.BranchingStub(self._channel)
        self.proposals = rpc.ProposalsStub(self._channel)
        self.admin = rpc.AdminStub(self._channel)

    def close(self) -> None:
        self._channel.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def repo(self, name: str) -> "Repo":
        return Repo(self, name)


@dataclass
class Repo:
    client: Client
    name: str

    def table(self, physical: str) -> "Table":
        return Table(self.client, self.name, physical)

    def create_branch(self, name: str, *, frm: str = "main") -> None:
        _call(
            self.client.branching.CreateBranch,
            pb.CreateBranchRequest(repo=self.name, name=name, **{"from": frm}),
            self.client,
        )


@dataclass
class Table:
    client: Client
    repo: str
    physical: str

    def read(
        self,
        *,
        branch: str = "main",
        where: pb.Expr | None = None,
        limit: int = 0,
    ) -> Iterator[Mapping[int, Any]]:
        """Stream rows from a branch.

        Reads on `main` do not need DataGit at all -- query the table directly.
        This exists for reading a BRANCH, or a point in history.
        """
        req = pb.ScanRequest(
            repo=self.repo, table=self.physical, branch=branch, limit=limit
        )
        if where is not None:
            req.filter.CopyFrom(where)
        try:
            for row in self.client.data.Scan(req, metadata=self.client._meta):
                yield {k: from_wire(v) for k, v in row.cells.items()}
        except grpc.RpcError as e:
            raise DataGitError(e) from None

    @contextlib.contextmanager
    def transaction(self, *, branch: str = "main", message: str) -> Iterator["Transaction"]:
        """Buffer changes and commit them as ONE commit on exit.

        Buffering is not just ergonomics: a commit takes the branch's ref lock,
        so throughput is commits per second regardless of how many rows each one
        carries (§11.3). One commit of a thousand rows costs what one commit of
        one row costs.

        An exception inside the block abandons the buffer without committing.
        """
        tx = Transaction(self, branch, message)
        yield tx
        tx.commit()


@dataclass
class Transaction:
    table: Table
    branch: str
    message: str
    _changes: list[pb.Change] = field(default_factory=list)

    def insert(self, pk: bytes, row: Mapping[int, Any]) -> None:
        self._changes.append(
            pb.Change(pk=pk, op=pb.OP_INSERT, row=_row(row))
        )

    def update(self, pk: bytes, row: Mapping[int, Any]) -> None:
        self._changes.append(
            pb.Change(pk=pk, op=pb.OP_UPDATE, row=_row(row))
        )

    def delete(self, pk: bytes) -> None:
        self._changes.append(pb.Change(pk=pk, op=pb.OP_DELETE))

    def commit(self) -> bytes:
        """Write the buffer as one commit and return its id.

        There is no author argument, and there never will be: the author comes
        from the credential this client authenticated with (§15.2).
        """
        if not self._changes:
            return b""
        res = _call(
            self.table.client.version.Commit,
            pb.CommitRequest(
                repo=self.table.repo,
                table=self.table.physical,
                branch=self.branch,
                message=self.message,
                changes=self._changes,
            ),
            self.table.client,
        )
        self._changes.clear()
        return res.commit_id


def _row(cells: Mapping[int, Any]) -> pb.Row:
    return pb.Row(cells={k: to_wire(v) for k, v in cells.items()})


def _call(stub_method: Any, req: Any, client: Client) -> Any:
    try:
        return stub_method(req, metadata=client._meta)
    except grpc.RpcError as e:
        raise DataGitError(e) from None
