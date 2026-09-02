import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CompareOp(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    COMPARE_OP_UNSPECIFIED: _ClassVar[CompareOp]
    COMPARE_OP_EQ: _ClassVar[CompareOp]
    COMPARE_OP_NE: _ClassVar[CompareOp]
    COMPARE_OP_LT: _ClassVar[CompareOp]
    COMPARE_OP_LE: _ClassVar[CompareOp]
    COMPARE_OP_GT: _ClassVar[CompareOp]
    COMPARE_OP_GE: _ClassVar[CompareOp]
    COMPARE_OP_LIKE: _ClassVar[CompareOp]

class Op(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    OP_UNSPECIFIED: _ClassVar[Op]
    OP_INSERT: _ClassVar[Op]
    OP_UPDATE: _ClassVar[Op]
    OP_DELETE: _ClassVar[Op]
COMPARE_OP_UNSPECIFIED: CompareOp
COMPARE_OP_EQ: CompareOp
COMPARE_OP_NE: CompareOp
COMPARE_OP_LT: CompareOp
COMPARE_OP_LE: CompareOp
COMPARE_OP_GT: CompareOp
COMPARE_OP_GE: CompareOp
COMPARE_OP_LIKE: CompareOp
OP_UNSPECIFIED: Op
OP_INSERT: Op
OP_UPDATE: Op
OP_DELETE: Op

class Value(_message.Message):
    __slots__ = ("is_null", "bool_value", "int_value", "float_value", "numeric_value", "text_value", "bytes_value", "time_value")
    IS_NULL_FIELD_NUMBER: _ClassVar[int]
    BOOL_VALUE_FIELD_NUMBER: _ClassVar[int]
    INT_VALUE_FIELD_NUMBER: _ClassVar[int]
    FLOAT_VALUE_FIELD_NUMBER: _ClassVar[int]
    NUMERIC_VALUE_FIELD_NUMBER: _ClassVar[int]
    TEXT_VALUE_FIELD_NUMBER: _ClassVar[int]
    BYTES_VALUE_FIELD_NUMBER: _ClassVar[int]
    TIME_VALUE_FIELD_NUMBER: _ClassVar[int]
    is_null: bool
    bool_value: bool
    int_value: int
    float_value: float
    numeric_value: str
    text_value: str
    bytes_value: bytes
    time_value: _timestamp_pb2.Timestamp
    def __init__(self, is_null: _Optional[bool] = ..., bool_value: _Optional[bool] = ..., int_value: _Optional[int] = ..., float_value: _Optional[float] = ..., numeric_value: _Optional[str] = ..., text_value: _Optional[str] = ..., bytes_value: _Optional[bytes] = ..., time_value: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class Row(_message.Message):
    __slots__ = ("cells",)
    class CellsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: int
        value: Value
        def __init__(self, key: _Optional[int] = ..., value: _Optional[_Union[Value, _Mapping]] = ...) -> None: ...
    CELLS_FIELD_NUMBER: _ClassVar[int]
    cells: _containers.MessageMap[int, Value]
    def __init__(self, cells: _Optional[_Mapping[int, Value]] = ...) -> None: ...

class Expr(_message.Message):
    __slots__ = ("compare", "is_null")
    COMPARE_FIELD_NUMBER: _ClassVar[int]
    IN_FIELD_NUMBER: _ClassVar[int]
    IS_NULL_FIELD_NUMBER: _ClassVar[int]
    AND_FIELD_NUMBER: _ClassVar[int]
    OR_FIELD_NUMBER: _ClassVar[int]
    NOT_FIELD_NUMBER: _ClassVar[int]
    compare: Compare
    is_null: IsNull
    def __init__(self, compare: _Optional[_Union[Compare, _Mapping]] = ..., is_null: _Optional[_Union[IsNull, _Mapping]] = ..., **kwargs) -> None: ...

class Compare(_message.Message):
    __slots__ = ("col", "op", "value")
    COL_FIELD_NUMBER: _ClassVar[int]
    OP_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    col: int
    op: CompareOp
    value: Value
    def __init__(self, col: _Optional[int] = ..., op: _Optional[_Union[CompareOp, str]] = ..., value: _Optional[_Union[Value, _Mapping]] = ...) -> None: ...

class In(_message.Message):
    __slots__ = ("col", "values")
    COL_FIELD_NUMBER: _ClassVar[int]
    VALUES_FIELD_NUMBER: _ClassVar[int]
    col: int
    values: _containers.RepeatedCompositeFieldContainer[Value]
    def __init__(self, col: _Optional[int] = ..., values: _Optional[_Iterable[_Union[Value, _Mapping]]] = ...) -> None: ...

class IsNull(_message.Message):
    __slots__ = ("col",)
    COL_FIELD_NUMBER: _ClassVar[int]
    col: int
    def __init__(self, col: _Optional[int] = ...) -> None: ...

class Junction(_message.Message):
    __slots__ = ("terms",)
    TERMS_FIELD_NUMBER: _ClassVar[int]
    terms: _containers.RepeatedCompositeFieldContainer[Expr]
    def __init__(self, terms: _Optional[_Iterable[_Union[Expr, _Mapping]]] = ...) -> None: ...

class Change(_message.Message):
    __slots__ = ("pk", "op", "row")
    PK_FIELD_NUMBER: _ClassVar[int]
    OP_FIELD_NUMBER: _ClassVar[int]
    ROW_FIELD_NUMBER: _ClassVar[int]
    pk: bytes
    op: Op
    row: Row
    def __init__(self, pk: _Optional[bytes] = ..., op: _Optional[_Union[Op, str]] = ..., row: _Optional[_Union[Row, _Mapping]] = ...) -> None: ...

class CommitInfo(_message.Message):
    __slots__ = ("id", "seq", "parents", "author", "committed_at", "message", "external_ref", "integrity")
    ID_FIELD_NUMBER: _ClassVar[int]
    SEQ_FIELD_NUMBER: _ClassVar[int]
    PARENTS_FIELD_NUMBER: _ClassVar[int]
    AUTHOR_FIELD_NUMBER: _ClassVar[int]
    COMMITTED_AT_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    EXTERNAL_REF_FIELD_NUMBER: _ClassVar[int]
    INTEGRITY_FIELD_NUMBER: _ClassVar[int]
    id: bytes
    seq: int
    parents: _containers.RepeatedScalarFieldContainer[bytes]
    author: str
    committed_at: _timestamp_pb2.Timestamp
    message: str
    external_ref: str
    integrity: str
    def __init__(self, id: _Optional[bytes] = ..., seq: _Optional[int] = ..., parents: _Optional[_Iterable[bytes]] = ..., author: _Optional[str] = ..., committed_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., message: _Optional[str] = ..., external_ref: _Optional[str] = ..., integrity: _Optional[str] = ...) -> None: ...

class Empty(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class CreateRepoRequest(_message.Message):
    __slots__ = ("name",)
    NAME_FIELD_NUMBER: _ClassVar[int]
    name: str
    def __init__(self, name: _Optional[str] = ...) -> None: ...

class RepoInfo(_message.Message):
    __slots__ = ("id", "name", "default_branch")
    ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_BRANCH_FIELD_NUMBER: _ClassVar[int]
    id: str
    name: str
    default_branch: str
    def __init__(self, id: _Optional[str] = ..., name: _Optional[str] = ..., default_branch: _Optional[str] = ...) -> None: ...

class TrackTableRequest(_message.Message):
    __slots__ = ("repo", "table", "mode")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    mode: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., mode: _Optional[str] = ...) -> None: ...

class ColumnInfo(_message.Message):
    __slots__ = ("id", "name", "sql_type", "nullable", "primary_key")
    ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    SQL_TYPE_FIELD_NUMBER: _ClassVar[int]
    NULLABLE_FIELD_NUMBER: _ClassVar[int]
    PRIMARY_KEY_FIELD_NUMBER: _ClassVar[int]
    id: int
    name: str
    sql_type: str
    nullable: bool
    primary_key: bool
    def __init__(self, id: _Optional[int] = ..., name: _Optional[str] = ..., sql_type: _Optional[str] = ..., nullable: _Optional[bool] = ..., primary_key: _Optional[bool] = ...) -> None: ...

class TableInfo(_message.Message):
    __slots__ = ("id", "name", "mode", "state", "columns")
    ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    COLUMNS_FIELD_NUMBER: _ClassVar[int]
    id: int
    name: str
    mode: str
    state: str
    columns: _containers.RepeatedCompositeFieldContainer[ColumnInfo]
    def __init__(self, id: _Optional[int] = ..., name: _Optional[str] = ..., mode: _Optional[str] = ..., state: _Optional[str] = ..., columns: _Optional[_Iterable[_Union[ColumnInfo, _Mapping]]] = ...) -> None: ...

class UntrackTableRequest(_message.Message):
    __slots__ = ("repo", "table")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ...) -> None: ...

class GetStatusRequest(_message.Message):
    __slots__ = ("repo",)
    REPO_FIELD_NUMBER: _ClassVar[int]
    repo: str
    def __init__(self, repo: _Optional[str] = ...) -> None: ...

class RepoStatus(_message.Message):
    __slots__ = ("repo", "tables", "refs")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLES_FIELD_NUMBER: _ClassVar[int]
    REFS_FIELD_NUMBER: _ClassVar[int]
    repo: RepoInfo
    tables: _containers.RepeatedCompositeFieldContainer[TableInfo]
    refs: _containers.RepeatedCompositeFieldContainer[RefInfo]
    def __init__(self, repo: _Optional[_Union[RepoInfo, _Mapping]] = ..., tables: _Optional[_Iterable[_Union[TableInfo, _Mapping]]] = ..., refs: _Optional[_Iterable[_Union[RefInfo, _Mapping]]] = ...) -> None: ...

class GetRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "pk", "at_commit", "as_of")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    PK_FIELD_NUMBER: _ClassVar[int]
    AT_COMMIT_FIELD_NUMBER: _ClassVar[int]
    AS_OF_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    pk: bytes
    at_commit: bytes
    as_of: _timestamp_pb2.Timestamp
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., pk: _Optional[bytes] = ..., at_commit: _Optional[bytes] = ..., as_of: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ScanRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "filter", "limit", "after", "at_commit", "as_of")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    FILTER_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    AFTER_FIELD_NUMBER: _ClassVar[int]
    AT_COMMIT_FIELD_NUMBER: _ClassVar[int]
    AS_OF_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    filter: Expr
    limit: int
    after: bytes
    at_commit: bytes
    as_of: _timestamp_pb2.Timestamp
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., filter: _Optional[_Union[Expr, _Mapping]] = ..., limit: _Optional[int] = ..., after: _Optional[bytes] = ..., at_commit: _Optional[bytes] = ..., as_of: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class CommitRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "changes", "message", "external_ref", "expected_head", "idempotency_key")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    CHANGES_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    EXTERNAL_REF_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_HEAD_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    changes: _containers.RepeatedCompositeFieldContainer[Change]
    message: str
    external_ref: str
    expected_head: bytes
    idempotency_key: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., changes: _Optional[_Iterable[_Union[Change, _Mapping]]] = ..., message: _Optional[str] = ..., external_ref: _Optional[str] = ..., expected_head: _Optional[bytes] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class CommitResponse(_message.Message):
    __slots__ = ("id", "seq", "rows_changed")
    ID_FIELD_NUMBER: _ClassVar[int]
    SEQ_FIELD_NUMBER: _ClassVar[int]
    ROWS_CHANGED_FIELD_NUMBER: _ClassVar[int]
    id: bytes
    seq: int
    rows_changed: int
    def __init__(self, id: _Optional[bytes] = ..., seq: _Optional[int] = ..., rows_changed: _Optional[int] = ...) -> None: ...

class LogRequest(_message.Message):
    __slots__ = ("repo", "branch", "limit")
    REPO_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    repo: str
    branch: str
    limit: int
    def __init__(self, repo: _Optional[str] = ..., branch: _Optional[str] = ..., limit: _Optional[int] = ...) -> None: ...

class DiffRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "from_seq", "to_seq")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    FROM_SEQ_FIELD_NUMBER: _ClassVar[int]
    TO_SEQ_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    from_seq: int
    to_seq: int
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., from_seq: _Optional[int] = ..., to_seq: _Optional[int] = ...) -> None: ...

class ChangeDetail(_message.Message):
    __slots__ = ("pk", "op", "before", "after", "changed_columns")
    PK_FIELD_NUMBER: _ClassVar[int]
    OP_FIELD_NUMBER: _ClassVar[int]
    BEFORE_FIELD_NUMBER: _ClassVar[int]
    AFTER_FIELD_NUMBER: _ClassVar[int]
    CHANGED_COLUMNS_FIELD_NUMBER: _ClassVar[int]
    pk: bytes
    op: Op
    before: Row
    after: Row
    changed_columns: _containers.RepeatedScalarFieldContainer[int]
    def __init__(self, pk: _Optional[bytes] = ..., op: _Optional[_Union[Op, str]] = ..., before: _Optional[_Union[Row, _Mapping]] = ..., after: _Optional[_Union[Row, _Mapping]] = ..., changed_columns: _Optional[_Iterable[int]] = ...) -> None: ...

class BlameRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "pk", "columns")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    PK_FIELD_NUMBER: _ClassVar[int]
    COLUMNS_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    pk: bytes
    columns: _containers.RepeatedScalarFieldContainer[int]
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., pk: _Optional[bytes] = ..., columns: _Optional[_Iterable[int]] = ...) -> None: ...

class CellBlame(_message.Message):
    __slots__ = ("col", "value", "commit_id", "author", "at", "message")
    COL_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    COMMIT_ID_FIELD_NUMBER: _ClassVar[int]
    AUTHOR_FIELD_NUMBER: _ClassVar[int]
    AT_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    col: int
    value: Value
    commit_id: bytes
    author: str
    at: _timestamp_pb2.Timestamp
    message: str
    def __init__(self, col: _Optional[int] = ..., value: _Optional[_Union[Value, _Mapping]] = ..., commit_id: _Optional[bytes] = ..., author: _Optional[str] = ..., at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., message: _Optional[str] = ...) -> None: ...

class HistoryRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "pk")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    PK_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    pk: bytes
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., pk: _Optional[bytes] = ...) -> None: ...

class RowVersion(_message.Message):
    __slots__ = ("seq_from", "seq_to", "op", "commit_id", "row", "author", "at", "message")
    SEQ_FROM_FIELD_NUMBER: _ClassVar[int]
    SEQ_TO_FIELD_NUMBER: _ClassVar[int]
    OP_FIELD_NUMBER: _ClassVar[int]
    COMMIT_ID_FIELD_NUMBER: _ClassVar[int]
    ROW_FIELD_NUMBER: _ClassVar[int]
    AUTHOR_FIELD_NUMBER: _ClassVar[int]
    AT_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    seq_from: int
    seq_to: int
    op: Op
    commit_id: bytes
    row: Row
    author: str
    at: _timestamp_pb2.Timestamp
    message: str
    def __init__(self, seq_from: _Optional[int] = ..., seq_to: _Optional[int] = ..., op: _Optional[_Union[Op, str]] = ..., commit_id: _Optional[bytes] = ..., row: _Optional[_Union[Row, _Mapping]] = ..., author: _Optional[str] = ..., at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., message: _Optional[str] = ...) -> None: ...

class RevertRequest(_message.Message):
    __slots__ = ("repo", "table", "branch", "commit_id", "message", "force")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    COMMIT_ID_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    FORCE_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    commit_id: bytes
    message: str
    force: bool
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ..., commit_id: _Optional[bytes] = ..., message: _Optional[str] = ..., force: _Optional[bool] = ...) -> None: ...

class CreateBranchRequest(_message.Message):
    __slots__ = ("repo", "name")
    REPO_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    repo: str
    name: str
    def __init__(self, repo: _Optional[str] = ..., name: _Optional[str] = ..., **kwargs) -> None: ...

class RefInfo(_message.Message):
    __slots__ = ("id", "kind", "name", "head", "head_seq", "parent", "chain_depth", "protected", "min_approvals", "merge_in_progress")
    ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    HEAD_FIELD_NUMBER: _ClassVar[int]
    HEAD_SEQ_FIELD_NUMBER: _ClassVar[int]
    PARENT_FIELD_NUMBER: _ClassVar[int]
    CHAIN_DEPTH_FIELD_NUMBER: _ClassVar[int]
    PROTECTED_FIELD_NUMBER: _ClassVar[int]
    MIN_APPROVALS_FIELD_NUMBER: _ClassVar[int]
    MERGE_IN_PROGRESS_FIELD_NUMBER: _ClassVar[int]
    id: str
    kind: str
    name: str
    head: bytes
    head_seq: int
    parent: str
    chain_depth: int
    protected: bool
    min_approvals: int
    merge_in_progress: bool
    def __init__(self, id: _Optional[str] = ..., kind: _Optional[str] = ..., name: _Optional[str] = ..., head: _Optional[bytes] = ..., head_seq: _Optional[int] = ..., parent: _Optional[str] = ..., chain_depth: _Optional[int] = ..., protected: _Optional[bool] = ..., min_approvals: _Optional[int] = ..., merge_in_progress: _Optional[bool] = ...) -> None: ...

class DeleteBranchRequest(_message.Message):
    __slots__ = ("repo", "name")
    REPO_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    repo: str
    name: str
    def __init__(self, repo: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class ListRefsRequest(_message.Message):
    __slots__ = ("repo",)
    REPO_FIELD_NUMBER: _ClassVar[int]
    repo: str
    def __init__(self, repo: _Optional[str] = ...) -> None: ...

class CreateTagRequest(_message.Message):
    __slots__ = ("repo", "name", "at_commit")
    REPO_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    AT_COMMIT_FIELD_NUMBER: _ClassVar[int]
    repo: str
    name: str
    at_commit: bytes
    def __init__(self, repo: _Optional[str] = ..., name: _Optional[str] = ..., at_commit: _Optional[bytes] = ...) -> None: ...

class UpdateFromParentRequest(_message.Message):
    __slots__ = ("repo", "table", "branch")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ...) -> None: ...

class MaterializeRequest(_message.Message):
    __slots__ = ("repo", "branch", "into_schema")
    REPO_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    INTO_SCHEMA_FIELD_NUMBER: _ClassVar[int]
    repo: str
    branch: str
    into_schema: str
    def __init__(self, repo: _Optional[str] = ..., branch: _Optional[str] = ..., into_schema: _Optional[str] = ...) -> None: ...

class ProtectRequest(_message.Message):
    __slots__ = ("repo", "branch", "protected", "min_approvals")
    REPO_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    PROTECTED_FIELD_NUMBER: _ClassVar[int]
    MIN_APPROVALS_FIELD_NUMBER: _ClassVar[int]
    repo: str
    branch: str
    protected: bool
    min_approvals: int
    def __init__(self, repo: _Optional[str] = ..., branch: _Optional[str] = ..., protected: _Optional[bool] = ..., min_approvals: _Optional[int] = ...) -> None: ...

class OpenSessionRequest(_message.Message):
    __slots__ = ("repo", "branch")
    REPO_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    repo: str
    branch: str
    def __init__(self, repo: _Optional[str] = ..., branch: _Optional[str] = ...) -> None: ...

class SessionInfo(_message.Message):
    __slots__ = ("id", "branch", "base_commit", "lease_until")
    ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    BASE_COMMIT_FIELD_NUMBER: _ClassVar[int]
    LEASE_UNTIL_FIELD_NUMBER: _ClassVar[int]
    id: str
    branch: str
    base_commit: bytes
    lease_until: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., branch: _Optional[str] = ..., base_commit: _Optional[bytes] = ..., lease_until: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class SessionWriteRequest(_message.Message):
    __slots__ = ("repo", "table", "session_id", "changes")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    CHANGES_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    session_id: str
    changes: _containers.RepeatedCompositeFieldContainer[Change]
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., session_id: _Optional[str] = ..., changes: _Optional[_Iterable[_Union[Change, _Mapping]]] = ...) -> None: ...

class CommitSessionRequest(_message.Message):
    __slots__ = ("repo", "table", "session_id", "message")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    session_id: str
    message: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., session_id: _Optional[str] = ..., message: _Optional[str] = ...) -> None: ...

class AbandonSessionRequest(_message.Message):
    __slots__ = ("repo", "table", "session_id")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    session_id: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., session_id: _Optional[str] = ...) -> None: ...

class CreateProposalRequest(_message.Message):
    __slots__ = ("repo", "into", "title", "description")
    REPO_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    INTO_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    repo: str
    into: str
    title: str
    description: str
    def __init__(self, repo: _Optional[str] = ..., into: _Optional[str] = ..., title: _Optional[str] = ..., description: _Optional[str] = ..., **kwargs) -> None: ...

class ProposalInfo(_message.Message):
    __slots__ = ("id", "into", "title", "state", "created_by", "created_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    INTO_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    CREATED_BY_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    id: int
    into: str
    title: str
    state: str
    created_by: str
    created_at: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[int] = ..., into: _Optional[str] = ..., title: _Optional[str] = ..., state: _Optional[str] = ..., created_by: _Optional[str] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., **kwargs) -> None: ...

class ReviewRequest(_message.Message):
    __slots__ = ("repo", "proposal_id", "kind", "body")
    REPO_FIELD_NUMBER: _ClassVar[int]
    PROPOSAL_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    repo: str
    proposal_id: int
    kind: str
    body: str
    def __init__(self, repo: _Optional[str] = ..., proposal_id: _Optional[int] = ..., kind: _Optional[str] = ..., body: _Optional[str] = ...) -> None: ...

class ListConflictsRequest(_message.Message):
    __slots__ = ("repo", "table", "proposal_id")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    PROPOSAL_ID_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    proposal_id: int
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., proposal_id: _Optional[int] = ...) -> None: ...

class ConflictInfo(_message.Message):
    __slots__ = ("id", "pk", "column", "kind", "base", "ours", "theirs", "resolved")
    ID_FIELD_NUMBER: _ClassVar[int]
    PK_FIELD_NUMBER: _ClassVar[int]
    COLUMN_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    BASE_FIELD_NUMBER: _ClassVar[int]
    OURS_FIELD_NUMBER: _ClassVar[int]
    THEIRS_FIELD_NUMBER: _ClassVar[int]
    RESOLVED_FIELD_NUMBER: _ClassVar[int]
    id: int
    pk: bytes
    column: str
    kind: str
    base: str
    ours: str
    theirs: str
    resolved: bool
    def __init__(self, id: _Optional[int] = ..., pk: _Optional[bytes] = ..., column: _Optional[str] = ..., kind: _Optional[str] = ..., base: _Optional[str] = ..., ours: _Optional[str] = ..., theirs: _Optional[str] = ..., resolved: _Optional[bool] = ...) -> None: ...

class ResolveConflictRequest(_message.Message):
    __slots__ = ("repo", "conflict_id", "resolution", "value")
    REPO_FIELD_NUMBER: _ClassVar[int]
    CONFLICT_ID_FIELD_NUMBER: _ClassVar[int]
    RESOLUTION_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    repo: str
    conflict_id: int
    resolution: str
    value: str
    def __init__(self, repo: _Optional[str] = ..., conflict_id: _Optional[int] = ..., resolution: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

class MergeProposalRequest(_message.Message):
    __slots__ = ("repo", "table", "proposal_id", "allow_chunked")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    PROPOSAL_ID_FIELD_NUMBER: _ClassVar[int]
    ALLOW_CHUNKED_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    proposal_id: int
    allow_chunked: bool
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., proposal_id: _Optional[int] = ..., allow_chunked: _Optional[bool] = ...) -> None: ...

class MergeResponse(_message.Message):
    __slots__ = ("clean", "commit_id", "rows_applied", "conflicts")
    CLEAN_FIELD_NUMBER: _ClassVar[int]
    COMMIT_ID_FIELD_NUMBER: _ClassVar[int]
    ROWS_APPLIED_FIELD_NUMBER: _ClassVar[int]
    CONFLICTS_FIELD_NUMBER: _ClassVar[int]
    clean: bool
    commit_id: bytes
    rows_applied: int
    conflicts: _containers.RepeatedCompositeFieldContainer[ConflictInfo]
    def __init__(self, clean: _Optional[bool] = ..., commit_id: _Optional[bytes] = ..., rows_applied: _Optional[int] = ..., conflicts: _Optional[_Iterable[_Union[ConflictInfo, _Mapping]]] = ...) -> None: ...

class PruneRequest(_message.Message):
    __slots__ = ("repo", "table", "keep_days", "keep_commits")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    KEEP_DAYS_FIELD_NUMBER: _ClassVar[int]
    KEEP_COMMITS_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    keep_days: int
    keep_commits: int
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., keep_days: _Optional[int] = ..., keep_commits: _Optional[int] = ...) -> None: ...

class PruneResponse(_message.Message):
    __slots__ = ("versions_removed", "commits_protected")
    VERSIONS_REMOVED_FIELD_NUMBER: _ClassVar[int]
    COMMITS_PROTECTED_FIELD_NUMBER: _ClassVar[int]
    versions_removed: int
    commits_protected: int
    def __init__(self, versions_removed: _Optional[int] = ..., commits_protected: _Optional[int] = ...) -> None: ...

class RunGCRequest(_message.Message):
    __slots__ = ("repo",)
    REPO_FIELD_NUMBER: _ClassVar[int]
    repo: str
    def __init__(self, repo: _Optional[str] = ...) -> None: ...

class GCResponse(_message.Message):
    __slots__ = ("orphan_versions", "sessions_reaped")
    ORPHAN_VERSIONS_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_REAPED_FIELD_NUMBER: _ClassVar[int]
    orphan_versions: int
    sessions_reaped: int
    def __init__(self, orphan_versions: _Optional[int] = ..., sessions_reaped: _Optional[int] = ...) -> None: ...

class PurgeRequest(_message.Message):
    __slots__ = ("repo", "table", "pk", "reason")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    PK_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    pk: bytes
    reason: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., pk: _Optional[bytes] = ..., reason: _Optional[str] = ...) -> None: ...

class PurgeReceipt(_message.Message):
    __slots__ = ("versions_removed", "commits_marked", "at")
    VERSIONS_REMOVED_FIELD_NUMBER: _ClassVar[int]
    COMMITS_MARKED_FIELD_NUMBER: _ClassVar[int]
    AT_FIELD_NUMBER: _ClassVar[int]
    versions_removed: int
    commits_marked: int
    at: _timestamp_pb2.Timestamp
    def __init__(self, versions_removed: _Optional[int] = ..., commits_marked: _Optional[int] = ..., at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class VerifyRequest(_message.Message):
    __slots__ = ("repo", "branch", "drift", "integrity", "intervals")
    REPO_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    DRIFT_FIELD_NUMBER: _ClassVar[int]
    INTEGRITY_FIELD_NUMBER: _ClassVar[int]
    INTERVALS_FIELD_NUMBER: _ClassVar[int]
    repo: str
    branch: str
    drift: bool
    integrity: bool
    intervals: bool
    def __init__(self, repo: _Optional[str] = ..., branch: _Optional[str] = ..., drift: _Optional[bool] = ..., integrity: _Optional[bool] = ..., intervals: _Optional[bool] = ...) -> None: ...

class VerifyFinding(_message.Message):
    __slots__ = ("check", "table", "ok", "detail")
    CHECK_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    OK_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    check: str
    table: str
    ok: bool
    detail: str
    def __init__(self, check: _Optional[str] = ..., table: _Optional[str] = ..., ok: _Optional[bool] = ..., detail: _Optional[str] = ...) -> None: ...

class ExportRequest(_message.Message):
    __slots__ = ("repo", "table", "branch")
    REPO_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    repo: str
    table: str
    branch: str
    def __init__(self, repo: _Optional[str] = ..., table: _Optional[str] = ..., branch: _Optional[str] = ...) -> None: ...

class ExportChunk(_message.Message):
    __slots__ = ("jsonl",)
    JSONL_FIELD_NUMBER: _ClassVar[int]
    jsonl: bytes
    def __init__(self, jsonl: _Optional[bytes] = ...) -> None: ...
