// Package adapter isolates every engine-specific decision behind one interface
// (DESIGN.md §4.3).
//
// The non-goal is as important as the goal: this layer does NOT hide real
// semantic differences. Where an engine cannot do something — MySQL and
// transactional DDL — the design changes to accommodate the weaker engine rather
// than pretending. Genuine differences are declared in Caps and surface to
// callers; they are never papered over here.
//
// A performance gap is not a capability difference and must not be recorded as
// one. Phase 0 finding: MySQL is expected to trail PostgreSQL on the resolution
// query shape, and the response is to measure and publish it (M5.3), with the
// §7.6 materialized-branch-heads fallback available per engine.
package adapter

import (
	"context"
	"time"

	"github.com/Glyph-Software/datagit/internal/core"
)

// Dialect names an engine.
type Dialect string

const (
	PostgreSQL Dialect = "postgres"
	MySQL      Dialect = "mysql"
)

// Caps is the capability matrix from DESIGN.md §4.3. Every entry exists because
// some code path branches on it.
type Caps struct {
	// TransactionalDDL is false on MySQL. It does not change the migration
	// algorithm: both engines run the same resumable journalled state machine
	// (§10.4), so failure behaviour is identical and only has to be tested once.
	// It exists so operators can be told the truth about what a crash costs.
	TransactionalDDL bool

	// DistinctOn selects the resolution query form. PostgreSQL has DISTINCT ON;
	// MySQL needs ROW_NUMBER() OVER (PARTITION BY ...). Both produce identical
	// results — asserted by the parity gate from M5 on.
	DistinctOn bool

	// TxnScopedAdvisoryLocks is false on MySQL, whose GET_LOCK is session-scoped.
	// The adapter releases explicitly on transaction end and a reaper clears
	// locks held by dead sessions (§11.3).
	TxnScopedAdvisoryLocks bool

	// PartialIndexes is false on MySQL, so the session index is partial on
	// PostgreSQL and a plain index there (§5.2).
	PartialIndexes bool

	// SupportedTypes lists the column types this engine can mirror into a typed
	// sidecar with a canonical encoding. A tracked table with a column outside
	// this set is refused for `versioned` mode, naming the column — DESIGN.md
	// §10.5 rule 5. Approximating instead would silently corrupt hashes.
	SupportedTypes map[string]core.Kind

	// MaterializedBranchHeads enables the §7.6 fallback for this engine: branch
	// heads are kept as materialized relations rather than resolved on the fly.
	// Per-engine, never a global design change, and off by default.
	MaterializedBranchHeads bool
}

// Tx is the subset of a database transaction the adapter needs. Keeping it
// narrow means the engine packages can be tested with fakes.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) error
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

type Row interface {
	Scan(dest ...any) error
}

// Segment is one link of a resolution chain (§7.3).
type Segment struct {
	BranchID [16]byte
	Seq      int64
}

// TableSpec describes a tracked table well enough to generate its sidecar.
type TableSpec struct {
	ID           uint64
	PhysicalName string
	Mode         Mode
	Columns      []Column
	PKColumns    []core.ColID
}

type Mode string

const (
	// ModeAudit records history but never branches, so it needs neither the
	// resolve nor the session index, and — Phase 0 finding F10 — must NOT take
	// the ref lock, which would cap it at the same ~850 commits/s as a branchable
	// table and make a machine-driven write rate unreachable.
	ModeAudit Mode = "audit"
	// ModeVersioned supports branching, diff, and three-way merge.
	ModeVersioned Mode = "versioned"
)

// Column is one mirrored column.
type Column struct {
	ID       core.ColID // stable; the sidecar column is named c_<ID> (§10.5)
	Name     string
	SQLType  string
	Kind     core.Kind
	Nullable bool
}

// ResolveSpec describes a resolution query.
type ResolveSpec struct {
	Table   *TableSpec
	Chain   []Segment
	Session *[16]byte // priority -1 segment when set

	// Filter is a typed predicate tree (§7.4). It is compiled to parameterized
	// SQL and applied to the RESOLVED row, never pushed into the union arms:
	// doing so resurfaces a parent's stale version for any row the branch edited
	// out of the predicate's range (§7.3). Only a primary-key predicate is safe
	// inside the arms, because row identity is immutable.
	Filter Expr

	// KeyFilter restricts resolution to specific primary keys, and IS pushed
	// into every union arm.
	//
	// That is safe only because a row's primary key is its identity for all of
	// history (§3.2, Phase 0 finding F6): no version of a row ever carries a
	// different key, so filtering by key cannot change which version wins. It
	// only stops the scan considering other keys. No value predicate has this
	// property, which is why Filter above is applied to the resolved row instead.
	KeyFilter Expr

	// Limit and After page the result. Phase 0 finding F9: each arm must be
	// ordered and limited individually, and the per-column index must end with
	// the primary key, or the page bounds the output without bounding the work.
	Limit int
	After core.PK
}

// Expr is a typed predicate node. There is deliberately no string form: with no
// SQL text to build, there is nothing to inject into (§15.4).
type Expr interface{ isExpr() }

type (
	// Compare is `col <op> value`.
	Compare struct {
		Col   core.ColID
		Op    CompareOp
		Value core.Value
	}
	// In is `col IN (values...)`.
	In struct {
		Col    core.ColID
		Values []core.Value
	}
	// IsNull is `col IS NULL`.
	IsNull struct{ Col core.ColID }
	// And, Or, Not compose.
	And struct{ Terms []Expr }
	Or  struct{ Terms []Expr }
	Not struct{ Term Expr }
)

func (Compare) isExpr() {}
func (In) isExpr()      {}
func (IsNull) isExpr()  {}
func (And) isExpr()     {}
func (Or) isExpr()      {}
func (Not) isExpr()     {}

type CompareOp uint8

const (
	Eq CompareOp = iota + 1
	Ne
	Lt
	Le
	Gt
	Ge
	Like
)

// Query is generated SQL plus its arguments. Adapters return this rather than
// executing, so query construction can be unit-tested without a database and so
// the parity gate can compare the two engines' output on identical input.
type Query struct {
	SQL  string
	Args []any
}

// MigrationOp is one step of a migration plan (§10.4).
type MigrationOp struct {
	Ordinal int
	Kind    string // add_column, backfill, add_index, drop_column, ...
	SQL     string
	Class   MigrationClass
	// Idempotent operations may be re-run after a crash. Every operation must be
	// written to be idempotent, because the journalled state machine resumes
	// rather than restarting (§10.4).
	Idempotent bool
}

type MigrationClass uint8

const (
	Additive MigrationClass = iota + 1
	Widening
	Narrowing
	Destructive
)

// MigrationPlan is a classified, ordered, resumable set of operations.
type MigrationPlan struct {
	TableID uint64
	Ops     []MigrationOp
}

// Adapter is the engine boundary.
type Adapter interface {
	Dialect() Dialect
	Caps() Caps

	// DDL for sidecars and materialization.
	CreateSidecar(ctx context.Context, tx Tx, t *TableSpec) error
	EvolveSidecar(ctx context.Context, tx Tx, from, to *TableSpec) error
	MaterializeBranch(ctx context.Context, tx Tx, chain []Segment, t *TableSpec, into string) error

	// Query construction. The resolution query differs materially per engine.
	ResolveQuery(spec *ResolveSpec) (Query, error)
	DiffQuery(t *TableSpec, branch [16]byte, fromSeq, toSeq int64) (Query, error)

	// Locking. AcquireRefLock is a no-op for audit-mode writes, which must not
	// serialize (finding F10).
	AcquireRefLock(ctx context.Context, tx Tx, ref [16]byte) error
	ReleaseRefLock(ctx context.Context, tx Tx, ref [16]byte) error

	// ApplyMigration runs a plan through the resumable journalled state machine.
	// Identical on both engines by design, so failure behaviour is tested once.
	ApplyMigration(ctx context.Context, plan *MigrationPlan, journal Journal) error

	// Now returns the database's clock. Commit timestamps come from here, never
	// from a DataGit replica, so `committed_at` is monotonic per branch
	// regardless of replica clock skew (§7.2).
	Now(ctx context.Context, tx Tx) (time.Time, error)
}

// Journal records migration progress so a crashed apply resumes rather than
// restarting or being left indeterminate (§10.4).
type Journal interface {
	Begin(ctx context.Context, plan *MigrationPlan) error
	MarkStarted(ctx context.Context, planID uint64, ordinal int) error
	MarkComplete(ctx context.Context, planID uint64, ordinal int) error
	Completed(ctx context.Context, planID uint64) (map[int]bool, error)
}
