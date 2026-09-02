// Package postgres implements the DataGit engine adapter for PostgreSQL
// (DESIGN.md §4.3). v1.0 ships this engine only; MySQL follows in v1.1.
package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
)

// MaxSeq is the open-interval sentinel. DESIGN.md §5.2d: an explicit sentinel
// rather than NULL, because NULL in a range predicate defeats the index.
const MaxSeq int64 = 9223372036854775807

// MaxChainDepth caps resolution segments (§18).
const MaxChainDepth = 8

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Dialect() adapter.Dialect { return adapter.PostgreSQL }

func (a *Adapter) Caps() adapter.Caps {
	return adapter.Caps{
		TransactionalDDL:       true,
		DistinctOn:             true,
		TxnScopedAdvisoryLocks: true,
		PartialIndexes:         true,
		SupportedTypes:         supportedTypes,
		// Off by default. Enabled per engine only if measurement requires it
		// (§7.6); PostgreSQL's Phase 0 numbers do not.
		MaterializedBranchHeads: false,
	}
}

// supportedTypes is the explicit allow-list from DESIGN.md §10.5 rule 5. A
// tracked table with a column outside it is refused for `versioned` mode, naming
// the column, rather than approximated — an approximation would silently produce
// unreproducible hashes.
var supportedTypes = map[string]core.Kind{
	"boolean":                     core.KindBool,
	"smallint":                    core.KindInt,
	"integer":                     core.KindInt,
	"bigint":                      core.KindInt,
	"real":                        core.KindFloat,
	"double precision":            core.KindFloat,
	"numeric":                     core.KindNumeric,
	"text":                        core.KindText,
	"character varying":           core.KindText,
	"character":                   core.KindText,
	"uuid":                        core.KindText,
	"bytea":                       core.KindBytes,
	"timestamp with time zone":    core.KindTime,
	"timestamp without time zone": core.KindTime,
	"date":                        core.KindTime,
}

// KindFor maps a PostgreSQL type name to a canonical kind, reporting whether the
// type can be mirrored at all.
func KindFor(sqlType string) (core.Kind, bool) {
	base := strings.ToLower(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	k, ok := supportedTypes[base]
	return k, ok
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// CreateSidecar generates and runs the sidecar DDL for a tracked table.
//
// Three indexes, not four (Phase 0 finding F11):
//   - resolve: the per-key lookup that serves point reads
//   - range:   interval scans for diff and historical reads. seq_to is
//     deliberately absent, because indexing it makes every
//     close-the-open-version UPDATE non-HOT and rewrites every index
//     entry for the row. seq_to is applied as a filter instead.
//   - session: partial, so it indexes only staged rows
//
// The commit_id index was dropped: a seq_from range on the range index answers
// "what did this commit change".
func (a *Adapter) CreateSidecar(ctx context.Context, tx adapter.Tx, t *adapter.TableSpec) error {
	sc := catalog.SidecarTable(t.PhysicalName)

	var cols []string
	for _, c := range t.Columns {
		null := ""
		if isPK(t, c.ID) {
			// Mirrored primary-key columns are NOT NULL; value columns are always
			// nullable, because a version may predate a column's existence.
			null = " NOT NULL"
		}
		cols = append(cols, fmt.Sprintf("    %s %s%s",
			quoteIdent(catalog.SidecarColumn(uint32(c.ID))), c.SQLType, null))
	}

	var pkCols []string
	for _, id := range t.PKColumns {
		pkCols = append(pkCols, quoteIdent(catalog.SidecarColumn(uint32(id))))
	}

	// Natural key, no surrogate. Phase 0 finding F8: PostgreSQL requires every
	// unique constraint on a partitioned table to contain the partition key, and
	// §14.3 partitions this table, so a bare `version_id bigserial PRIMARY KEY`
	// and partitioning cannot both hold. Nothing references a surrogate id.
	ddl := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    branch_id    uuid     NOT NULL,
    seq_from     bigint   NOT NULL,
    seq_to       bigint   NOT NULL DEFAULT %d,
    op           smallint NOT NULL,
    commit_id    bytea    NOT NULL,
    session_id   uuid,
    changed_cols bytea    NOT NULL,
%s,
    PRIMARY KEY (branch_id, %s, seq_from)
)`, quoteIdent(sc), MaxSeq, strings.Join(cols, ",\n"), strings.Join(pkCols, ", "))

	if err := tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create sidecar %s: %w", sc, err)
	}

	idx := []string{
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (branch_id, %s, seq_from DESC)`,
			quoteIdent("vx_"+t.PhysicalName+"_resolve"), quoteIdent(sc), strings.Join(pkCols, ", ")),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (branch_id, seq_from)`,
			quoteIdent("vx_"+t.PhysicalName+"_range"), quoteIdent(sc)),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (session_id) WHERE session_id IS NOT NULL`,
			quoteIdent("vx_"+t.PhysicalName+"_session"), quoteIdent(sc)),
	}
	for _, s := range idx {
		if err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("create sidecar index: %w", err)
		}
	}
	return nil
}

func isPK(t *adapter.TableSpec, id core.ColID) bool {
	for _, p := range t.PKColumns {
		if p == id {
			return true
		}
	}
	return false
}

// EvolveSidecar applies an additive or widening schema change. Narrowing forks
// to a new column id rather than coercing history (§10.5 rule 3); that lands
// with the schema engine in M6.
func (a *Adapter) EvolveSidecar(ctx context.Context, tx adapter.Tx, from, to *adapter.TableSpec) error {
	sc := catalog.SidecarTable(to.PhysicalName)
	have := map[core.ColID]adapter.Column{}
	for _, c := range from.Columns {
		have[c.ID] = c
	}
	for _, c := range to.Columns {
		old, existed := have[c.ID]
		switch {
		case !existed:
			// Additive: always safe, and always nullable in the sidecar.
			if err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`,
				quoteIdent(sc), quoteIdent(catalog.SidecarColumn(uint32(c.ID))), c.SQLType)); err != nil {
				return fmt.Errorf("evolve sidecar add %s: %w", c.Name, err)
			}
		case old.SQLType != c.SQLType:
			// Only widening is handled here. Anything else is a narrowing fork
			// and belongs to the schema engine.
			if err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE %s`,
				quoteIdent(sc), quoteIdent(catalog.SidecarColumn(uint32(c.ID))), c.SQLType)); err != nil {
				return fmt.Errorf("evolve sidecar widen %s (%s -> %s): %w",
					c.Name, old.SQLType, c.SQLType, err)
			}
		}
	}
	return nil
}

// --- Resolution (DESIGN.md §7.3) ---

// ResolveQuery builds the priority-fallthrough resolution query.
//
// Two hazards are structural here, both measured in Phase 0 at 51.4M versions:
//
//  1. `op <> 3` is filtered in the OUTER scope, never inside an arm. Inside, a
//     branch-level delete is dropped from its own arm and the parent's older
//     version resurfaces — 140,000 rows at depth 8.
//
//  2. A value filter is applied to the RESOLVED row, never inside an arm, and
//     the query is two-pass: candidate keys first, then full resolution of
//     exactly those keys. Inside, a row the branch edited out of the predicate's
//     range reappears from the parent — 1,400 spurious rows at depth 8.
//
// The one safe pushdown is the primary key, because row identity is immutable
// (§3.2): filtering by key inside every arm cannot change which version wins.
func (a *Adapter) ResolveQuery(spec *adapter.ResolveSpec) (adapter.Query, error) {
	if len(spec.Chain) == 0 {
		return adapter.Query{}, fmt.Errorf("postgres: empty resolution chain")
	}
	if len(spec.Chain) > MaxChainDepth {
		return adapter.Query{}, fmt.Errorf("postgres: chain depth %d exceeds the cap of %d",
			len(spec.Chain), MaxChainDepth)
	}
	b := &builder{spec: spec}
	return b.build()
}

type builder struct {
	spec *adapter.ResolveSpec
	args []any
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

func (b *builder) build() (adapter.Query, error) {
	t := b.spec.Table
	sc := quoteIdent(catalog.SidecarTable(t.PhysicalName))

	pkCols := make([]string, 0, len(t.PKColumns))
	for _, id := range t.PKColumns {
		pkCols = append(pkCols, quoteIdent(catalog.SidecarColumn(uint32(id))))
	}
	valCols := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		valCols = append(valCols, quoteIdent(catalog.SidecarColumn(uint32(c.ID))))
	}
	pkList := strings.Join(pkCols, ", ")

	// Pass 1: candidate keys, only when a value filter is present.
	var cte string
	if b.spec.Filter != nil {
		var arms []string
		for _, seg := range b.spec.Chain {
			where, err := b.armWhere(seg)
			if err != nil {
				return adapter.Query{}, err
			}
			filt, err := b.compile(b.spec.Filter, "v")
			if err != nil {
				return adapter.Query{}, err
			}
			arm := fmt.Sprintf(
				`SELECT %s FROM %s v WHERE %s AND v.op <> 3 AND (%s)`,
				prefixed("v", pkCols), sc, where, filt)
			// Phase 0 finding F9: each arm is ordered and limited individually.
			// Without it, the outer DISTINCT must aggregate the whole union
			// before it can sort and limit, so the page bounds the output but not
			// the work.
			if b.spec.Limit > 0 {
				arm = fmt.Sprintf("(%s%s ORDER BY %s LIMIT %d)",
					arm, b.afterClause("v", pkCols), prefixed("v", pkCols), b.spec.Limit*2)
			}
			arms = append(arms, arm)
		}
		inner := strings.Join(arms, "\n  UNION ALL\n  ")
		lim := ""
		if b.spec.Limit > 0 {
			lim = fmt.Sprintf(" ORDER BY %s LIMIT %d", pkList, b.spec.Limit*2)
		}
		cte = fmt.Sprintf("WITH cand AS (\n  SELECT DISTINCT %s FROM (\n  %s\n  ) k%s\n)\n",
			pkList, inner, lim)
	}

	// Pass 2: resolve, priority-ordered.
	var arms []string
	prio := 0
	if b.spec.Session != nil {
		// Priority -1: the session's own staged rows (§7.3, §6.2).
		arms = append(arms, fmt.Sprintf(
			`SELECT -1 AS prio, %s, v.op FROM %s v WHERE v.session_id = %s`,
			prefixed("v", valCols), sc, b.arg(*b.spec.Session)))
	}
	for _, seg := range b.spec.Chain {
		where, err := b.armWhere(seg)
		if err != nil {
			return adapter.Query{}, err
		}
		join := ""
		if b.spec.Filter != nil {
			join = fmt.Sprintf(" JOIN cand USING (%s)", pkList)
		}
		arms = append(arms, fmt.Sprintf(
			`SELECT %d AS prio, %s, v.op FROM %s v%s WHERE %s`,
			prio, prefixed("v", valCols), sc, join, where))
		prio++
	}

	outer := []string{"r.op <> 3"}
	if b.spec.Filter != nil {
		f, err := b.compile(b.spec.Filter, "r")
		if err != nil {
			return adapter.Query{}, err
		}
		outer = append(outer, "("+f+")")
	}
	tail := ""
	if b.spec.Limit > 0 {
		tail = fmt.Sprintf(" ORDER BY %s LIMIT %d", prefixed("r", pkCols), b.spec.Limit)
	}

	sql := fmt.Sprintf(`%sSELECT %s, r.op FROM (
  SELECT DISTINCT ON (%s) %s, s.op
  FROM (
  %s
  ) s
  ORDER BY %s, s.prio
) r
WHERE %s%s`,
		cte, prefixed("r", valCols),
		prefixed("s", pkCols), prefixed("s", valCols),
		strings.Join(arms, "\n  UNION ALL\n  "),
		prefixed("s", pkCols),
		strings.Join(outer, " AND "), tail)

	return adapter.Query{SQL: sql, Args: b.args}, nil
}

// armWhere is the per-segment interval predicate. Note what is NOT here: no
// tombstone filter and no value filter.
func (b *builder) armWhere(seg adapter.Segment) (string, error) {
	return fmt.Sprintf(
		"v.branch_id = %s AND v.session_id IS NULL AND v.seq_from <= %s AND v.seq_to > %s",
		b.arg(seg.BranchID), b.arg(seg.Seq), b.arg(seg.Seq)), nil
}

func (b *builder) afterClause(alias string, pkCols []string) string {
	if b.spec.After == "" {
		return ""
	}
	// Keyset pagination on the canonical primary key.
	return fmt.Sprintf(" AND %s > %s", prefixed(alias, pkCols), b.arg(string(b.spec.After)))
}

func prefixed(alias string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return strings.Join(out, ", ")
}

// compile turns a typed predicate into parameterized SQL. There is no string
// path: every literal becomes a bind parameter, so there is nothing to inject
// into (§15.4).
func (b *builder) compile(e adapter.Expr, alias string) (string, error) {
	switch n := e.(type) {
	case adapter.Compare:
		op, err := sqlOp(n.Op)
		if err != nil {
			return "", err
		}
		v, err := bindValue(n.Value)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s.%s %s %s", alias,
			quoteIdent(catalog.SidecarColumn(uint32(n.Col))), op, b.arg(v)), nil
	case adapter.In:
		if len(n.Values) == 0 {
			return "false", nil
		}
		vals := make([]any, 0, len(n.Values))
		for _, v := range n.Values {
			bv, err := bindValue(v)
			if err != nil {
				return "", err
			}
			vals = append(vals, bv)
		}
		return fmt.Sprintf("%s.%s = ANY(%s)", alias,
			quoteIdent(catalog.SidecarColumn(uint32(n.Col))), b.arg(vals)), nil
	case adapter.IsNull:
		return fmt.Sprintf("%s.%s IS NULL", alias,
			quoteIdent(catalog.SidecarColumn(uint32(n.Col)))), nil
	case adapter.And:
		return b.join(n.Terms, " AND ", alias, "true")
	case adapter.Or:
		return b.join(n.Terms, " OR ", alias, "false")
	case adapter.Not:
		inner, err := b.compile(n.Term, alias)
		if err != nil {
			return "", err
		}
		return "NOT (" + inner + ")", nil
	}
	return "", fmt.Errorf("postgres: unsupported predicate %T", e)
}

func (b *builder) join(terms []adapter.Expr, sep, alias, empty string) (string, error) {
	if len(terms) == 0 {
		return empty, nil
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		s, err := b.compile(t, alias)
		if err != nil {
			return "", err
		}
		parts = append(parts, "("+s+")")
	}
	return strings.Join(parts, sep), nil
}

func sqlOp(o adapter.CompareOp) (string, error) {
	switch o {
	case adapter.Eq:
		return "=", nil
	case adapter.Ne:
		return "<>", nil
	case adapter.Lt:
		return "<", nil
	case adapter.Le:
		return "<=", nil
	case adapter.Gt:
		return ">", nil
	case adapter.Ge:
		return ">=", nil
	case adapter.Like:
		return "LIKE", nil
	}
	return "", fmt.Errorf("postgres: unsupported comparison %d", o)
}

// bindValue converts a canonical value into something the driver can bind.
func bindValue(v core.Value) (any, error) {
	switch v.Kind {
	case core.KindNull:
		return nil, nil
	case core.KindBool:
		return v.Bool, nil
	case core.KindInt:
		return v.Int, nil
	case core.KindFloat:
		return v.Float, nil
	case core.KindNumeric:
		return v.Text, nil
	case core.KindText:
		return v.Text, nil
	case core.KindBytes:
		return v.Bytes, nil
	case core.KindTime:
		return v.AsTime(), nil
	}
	return nil, fmt.Errorf("postgres: cannot bind kind %s", v.Kind)
}

// DiffQuery is the interval scan from §8.1: only versions whose boundaries fall
// in the range are visited, so cost is proportional to the change.
func (a *Adapter) DiffQuery(t *adapter.TableSpec, branch [16]byte, fromSeq, toSeq int64) (adapter.Query, error) {
	sc := quoteIdent(catalog.SidecarTable(t.PhysicalName))
	cols := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		cols = append(cols, quoteIdent(catalog.SidecarColumn(uint32(c.ID))))
	}
	sql := fmt.Sprintf(
		`SELECT %s, op, seq_from FROM %s
          WHERE branch_id = $1 AND session_id IS NULL
            AND seq_from > $2 AND seq_from <= $3
          ORDER BY seq_from`,
		strings.Join(cols, ", "), sc)
	return adapter.Query{SQL: sql, Args: []any{branch, fromSeq, toSeq}}, nil
}

// --- Locking (§11.3) ---

// AcquireRefLock serializes commits on a branch.
//
// Phase 0 finding F10: this is a throughput ceiling, measured at ~850 commits/s
// per branch regardless of writer count. Audit-mode writes must NOT call it —
// they never branch, so they order by a sequence instead and are not serialized.
func (a *Adapter) AcquireRefLock(ctx context.Context, tx adapter.Tx, ref [16]byte) error {
	// Transaction-scoped on PostgreSQL, so it releases automatically on commit
	// or rollback. MySQL's GET_LOCK is session-scoped and needs explicit release.
	return tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, uuidString(ref))
}

func (a *Adapter) ReleaseRefLock(ctx context.Context, tx adapter.Tx, ref [16]byte) error {
	return nil // transaction-scoped: nothing to do
}

func (a *Adapter) Now(ctx context.Context, tx adapter.Tx) (time.Time, error) {
	var t time.Time
	// The database clock, never a DataGit replica's: commit timestamps must be
	// monotonic per branch regardless of replica clock skew (§7.2).
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func uuidString(u [16]byte) string {
	const hexd = "0123456789abcdef"
	buf := make([]byte, 0, 36)
	for i, b := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf = append(buf, '-')
		}
		buf = append(buf, hexd[b>>4], hexd[b&0xf])
	}
	return string(buf)
}

// MaterializeBranch and ApplyMigration land with M2.10 and M6 respectively.
func (a *Adapter) MaterializeBranch(ctx context.Context, tx adapter.Tx, chain []adapter.Segment, t *adapter.TableSpec, into string) error {
	return fmt.Errorf("postgres: MaterializeBranch lands in M2.10")
}

func (a *Adapter) ApplyMigration(ctx context.Context, plan *adapter.MigrationPlan, j adapter.Journal) error {
	return fmt.Errorf("postgres: ApplyMigration lands in M6")
}
