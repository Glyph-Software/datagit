// Package mysql implements the DataGit engine adapter for MySQL 8.4
// (DESIGN.md §4.3, M5 / v1.1).
//
// The differences from PostgreSQL that actually matter, each declared in Caps
// rather than papered over:
//
//   - No DISTINCT ON, so resolution uses ROW_NUMBER() OVER (PARTITION BY ...).
//     Both forms must return identical results; the parity gate asserts it.
//   - No transactional DDL. This does NOT change the migration algorithm: §10.4
//     runs the same resumable journalled state machine on both engines, and S4
//     verified convergence from every crash point on both.
//   - GET_LOCK is session-scoped rather than transaction-scoped, so the lock must
//     be released explicitly and a reaper must clear locks held by dead sessions.
//   - No partial indexes, so the session index is a plain index.
//
// A performance gap is not a capability difference and is not recorded here.
// MySQL is expected to trail on the resolution shape; the response is to measure
// and publish it (M5.3), with the §7.6 materialized-branch-heads fallback
// available per engine.
package mysql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
)

const MaxSeq int64 = 9223372036854775807

const MaxChainDepth = 8

type ExecFunc func(ctx context.Context, sql string) error

type Adapter struct{ exec ExecFunc }

func New() *Adapter                         { return &Adapter{} }
func NewWithExec(e ExecFunc) *Adapter       { return &Adapter{exec: e} }
func (a *Adapter) Dialect() adapter.Dialect { return adapter.MySQL }

func (a *Adapter) Caps() adapter.Caps {
	return adapter.Caps{
		TransactionalDDL:       false,
		DistinctOn:             false,
		TxnScopedAdvisoryLocks: false,
		PartialIndexes:         false,
		SupportedTypes:         supportedTypes,
		// Off until M5.3 measures MySQL against the S1 workloads. Turning it on
		// without measurement would be exactly the unmeasured leap Phase 0 exists
		// to prevent.
		MaterializedBranchHeads: false,
	}
}

// supportedTypes is MySQL's §10.5 rule 5 allow-list.
//
// ENUM and SET are deliberately absent: their value lists can diverge between
// branches, so a value that is legal on one branch may be illegal on another and
// the canonical encoding would not survive a merge.
var supportedTypes = map[string]core.Kind{
	"tinyint":    core.KindInt,
	"smallint":   core.KindInt,
	"mediumint":  core.KindInt,
	"int":        core.KindInt,
	"integer":    core.KindInt,
	"bigint":     core.KindInt,
	"float":      core.KindFloat,
	"double":     core.KindFloat,
	"decimal":    core.KindNumeric,
	"numeric":    core.KindNumeric,
	"char":       core.KindText,
	"varchar":    core.KindText,
	"text":       core.KindText,
	"tinytext":   core.KindText,
	"mediumtext": core.KindText,
	"longtext":   core.KindText,
	"binary":     core.KindBytes,
	"varbinary":  core.KindBytes,
	"blob":       core.KindBytes,
	"datetime":   core.KindTime,
	"timestamp":  core.KindTime,
	"date":       core.KindTime,
}

func KindFor(sqlType string) (core.Kind, bool) {
	base := strings.ToLower(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	base = strings.TrimSuffix(base, " unsigned")
	k, ok := supportedTypes[base]
	return k, ok
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// CreateSidecar mirrors the PostgreSQL layout, with two engine differences:
// the session index is plain rather than partial, and text primary-key columns
// need an explicit length in the key.
func (a *Adapter) CreateSidecar(ctx context.Context, tx adapter.Tx, t *adapter.TableSpec) error {
	sc := catalog.SidecarTable(t.PhysicalName)

	var cols []string
	for _, c := range t.Columns {
		null := ""
		if isPK(t, c.ID) {
			null = " NOT NULL"
		}
		cols = append(cols, fmt.Sprintf("  %s %s%s",
			quoteIdent(catalog.SidecarColumn(uint32(c.ID))), c.SQLType, null))
	}

	var pkParts []string
	for _, id := range t.PKColumns {
		col := quoteIdent(catalog.SidecarColumn(uint32(id)))
		// MySQL cannot index an unbounded TEXT/BLOB without a prefix length.
		if c, ok := column(t, id); ok && needsKeyPrefix(c.SQLType) {
			col += "(191)"
		}
		pkParts = append(pkParts, col)
	}

	ddl := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  branch_id    binary(16) NOT NULL,
  seq_from     bigint     NOT NULL,
  seq_to       bigint     NOT NULL DEFAULT %d,
  op           smallint   NOT NULL,
  commit_id    varbinary(32) NOT NULL,
  session_id   binary(16) NULL,
  changed_cols varbinary(255) NOT NULL,
%s,
  PRIMARY KEY (branch_id, %s, seq_from)
) ENGINE=InnoDB`, quoteIdent(sc), MaxSeq, strings.Join(cols, ",\n"), strings.Join(pkParts, ", "))

	if err := tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create sidecar %s: %w", sc, err)
	}

	// Three indexes, matching PostgreSQL. seq_to is deliberately absent from the
	// range index for the same reason (finding F11), and the session index is
	// plain because MySQL has no partial indexes.
	idx := []struct{ name, def string }{
		{"vx_" + t.PhysicalName + "_resolve",
			fmt.Sprintf("(branch_id, %s, seq_from DESC)", strings.Join(pkParts, ", "))},
		{"vx_" + t.PhysicalName + "_range", "(branch_id, seq_from)"},
		{"vx_" + t.PhysicalName + "_session", "(session_id)"},
	}
	for _, i := range idx {
		// MySQL has no CREATE INDEX IF NOT EXISTS, so existence is checked by
		// hand — the same hand-written idempotency S4 found this engine needs
		// throughout.
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.statistics
			  WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
			sc, i.name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := tx.Exec(ctx, fmt.Sprintf(`CREATE INDEX %s ON %s %s`,
			quoteIdent(i.name), quoteIdent(sc), i.def)); err != nil {
			return fmt.Errorf("create sidecar index %s: %w", i.name, err)
		}
	}
	return nil
}

func needsKeyPrefix(sqlType string) bool {
	base := strings.ToLower(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(base, '('); i >= 0 {
		return false // an explicit length is already present
	}
	switch base {
	case "text", "tinytext", "mediumtext", "longtext", "blob":
		return true
	}
	return false
}

func column(t *adapter.TableSpec, id core.ColID) (adapter.Column, bool) {
	for _, c := range t.Columns {
		if c.ID == id {
			return c, true
		}
	}
	return adapter.Column{}, false
}

func isPK(t *adapter.TableSpec, id core.ColID) bool {
	for _, p := range t.PKColumns {
		if p == id {
			return true
		}
	}
	return false
}

func (a *Adapter) EvolveSidecar(ctx context.Context, tx adapter.Tx, from, to *adapter.TableSpec) error {
	sc := catalog.SidecarTable(to.PhysicalName)
	have := map[core.ColID]adapter.Column{}
	for _, c := range from.Columns {
		have[c.ID] = c
	}
	for _, c := range to.Columns {
		old, existed := have[c.ID]
		name := catalog.SidecarColumn(uint32(c.ID))
		if !existed {
			// MySQL before 8.0.29 has no IF NOT EXISTS for columns, so check.
			var n int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM information_schema.columns
				  WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
				sc, name).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				continue
			}
			if err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`,
				quoteIdent(sc), quoteIdent(name), c.SQLType)); err != nil {
				return fmt.Errorf("evolve sidecar add %s: %w", c.Name, err)
			}
			continue
		}
		if old.SQLType != c.SQLType {
			if err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s MODIFY COLUMN %s %s`,
				quoteIdent(sc), quoteIdent(name), c.SQLType)); err != nil {
				return fmt.Errorf("evolve sidecar widen %s: %w", c.Name, err)
			}
		}
	}
	return nil
}

// ResolveQuery builds the resolution query using ROW_NUMBER(), MySQL's
// equivalent of DISTINCT ON.
//
// The two §7.3 hazards are structural here exactly as in PostgreSQL: the
// tombstone filter is applied in the OUTER scope, and a value filter is applied
// to the RESOLVED row via the two-pass form. Only a primary-key predicate is
// pushed into the arms, because row identity is immutable (finding F6).
func (a *Adapter) ResolveQuery(spec *adapter.ResolveSpec) (adapter.Query, error) {
	if len(spec.Chain) == 0 {
		return adapter.Query{}, fmt.Errorf("mysql: empty resolution chain")
	}
	if len(spec.Chain) > MaxChainDepth {
		return adapter.Query{}, fmt.Errorf("mysql: chain depth %d exceeds the cap of %d",
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
	return "?"
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
			if b.spec.Limit > 0 {
				arm = fmt.Sprintf("(%s ORDER BY %s LIMIT %d)",
					arm, prefixed("v", pkCols), b.spec.Limit*2)
			}
			arms = append(arms, arm)
		}
		lim := ""
		if b.spec.Limit > 0 {
			lim = fmt.Sprintf(" ORDER BY %s LIMIT %d", pkList, b.spec.Limit*2)
		}
		cte = fmt.Sprintf("WITH cand AS (\n  SELECT DISTINCT %s FROM (\n  %s\n  ) k%s\n)\n",
			pkList, strings.Join(arms, "\n  UNION ALL\n  "), lim)
	}

	var arms []string
	prio := 0
	if b.spec.Session != nil {
		sessWhere := fmt.Sprintf("v.session_id = %s", b.arg((*b.spec.Session)[:]))
		if b.spec.KeyFilter != nil {
			k, err := b.compile(b.spec.KeyFilter, "v")
			if err != nil {
				return adapter.Query{}, err
			}
			sessWhere += " AND (" + k + ")"
		}
		arms = append(arms, fmt.Sprintf(
			`SELECT -1 AS prio, %s, v.op FROM %s v WHERE %s`,
			prefixed("v", valCols), sc, sessWhere))
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

	outer := []string{"r.op <> 3", "r.rn = 1"}
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

	// ROW_NUMBER() in place of DISTINCT ON. The window partitions by primary key
	// and orders by priority, so rn = 1 is the winning segment's version.
	sql := fmt.Sprintf(`%sSELECT %s, r.op FROM (
  SELECT s.*, ROW_NUMBER() OVER (PARTITION BY %s ORDER BY s.prio) AS rn
  FROM (
  %s
  ) s
) r
WHERE %s%s`,
		cte, prefixed("r", valCols),
		prefixed("s", pkCols),
		strings.Join(arms, "\n  UNION ALL\n  "),
		strings.Join(outer, " AND "), tail)

	return adapter.Query{SQL: sql, Args: b.args}, nil
}

func (b *builder) armWhere(seg adapter.Segment) (string, error) {
	w := fmt.Sprintf(
		"v.branch_id = %s AND v.session_id IS NULL AND v.seq_from <= %s AND v.seq_to > %s",
		b.arg(seg.BranchID[:]), b.arg(seg.Seq), b.arg(seg.Seq))
	if b.spec.KeyFilter != nil {
		k, err := b.compile(b.spec.KeyFilter, "v")
		if err != nil {
			return "", err
		}
		w += " AND (" + k + ")"
	}
	return w, nil
}

func prefixed(alias string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return strings.Join(out, ", ")
}

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
		ph := make([]string, 0, len(n.Values))
		for _, v := range n.Values {
			bv, err := bindValue(v)
			if err != nil {
				return "", err
			}
			ph = append(ph, b.arg(bv))
		}
		return fmt.Sprintf("%s.%s IN (%s)", alias,
			quoteIdent(catalog.SidecarColumn(uint32(n.Col))), strings.Join(ph, ", ")), nil
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
	return "", fmt.Errorf("mysql: unsupported predicate %T", e)
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
	return "", fmt.Errorf("mysql: unsupported comparison %d", o)
}

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
	case core.KindNumeric, core.KindText:
		return v.Text, nil
	case core.KindBytes:
		return v.Bytes, nil
	case core.KindTime:
		return v.AsTime(), nil
	}
	return nil, fmt.Errorf("mysql: cannot bind kind %s", v.Kind)
}

func (a *Adapter) DiffQuery(t *adapter.TableSpec, branch [16]byte, fromSeq, toSeq int64) (adapter.Query, error) {
	sc := quoteIdent(catalog.SidecarTable(t.PhysicalName))
	cols := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		cols = append(cols, quoteIdent(catalog.SidecarColumn(uint32(c.ID))))
	}
	sql := fmt.Sprintf(
		`SELECT %s, op, seq_from FROM %s
          WHERE branch_id = ? AND session_id IS NULL
            AND seq_from > ? AND seq_from <= ?
          ORDER BY seq_from`,
		strings.Join(cols, ", "), sc)
	return adapter.Query{SQL: sql, Args: []any{branch[:], fromSeq, toSeq}}, nil
}

// AcquireRefLock uses GET_LOCK, which is SESSION-scoped on MySQL rather than
// transaction-scoped (§11.3). It must be released explicitly, and a reaper must
// clear locks held by dead sessions — MySQL releases them when the connection
// closes, but a pooled connection may outlive the transaction.
func (a *Adapter) AcquireRefLock(ctx context.Context, tx adapter.Tx, ref [16]byte) error {
	var got int
	if err := tx.QueryRow(ctx, `SELECT GET_LOCK(?, 10)`, lockName(ref)).Scan(&got); err != nil {
		return err
	}
	if got != 1 {
		return fmt.Errorf("mysql: timed out acquiring the ref lock")
	}
	return nil
}

func (a *Adapter) ReleaseRefLock(ctx context.Context, tx adapter.Tx, ref [16]byte) error {
	var released int
	return tx.QueryRow(ctx, `SELECT RELEASE_LOCK(?)`, lockName(ref)).Scan(&released)
}

func lockName(ref [16]byte) string {
	const hexd = "0123456789abcdef"
	buf := make([]byte, 0, 40)
	buf = append(buf, "datagit:"...)
	for _, b := range ref {
		buf = append(buf, hexd[b>>4], hexd[b&0xf])
	}
	return string(buf)
}

func (a *Adapter) Now(ctx context.Context, tx adapter.Tx) (time.Time, error) {
	var t time.Time
	// The database clock, at microsecond precision, matching the canonical
	// encoding (§7.2, §12.1).
	if err := tx.QueryRow(ctx, `SELECT NOW(6)`).Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func (a *Adapter) MaterializeBranch(ctx context.Context, tx adapter.Tx, chain []adapter.Segment, t *adapter.TableSpec, into string) error {
	q, err := a.ResolveQuery(&adapter.ResolveSpec{Table: t, Chain: chain})
	if err != nil {
		return err
	}
	cols := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		cols = append(cols, fmt.Sprintf("%s AS %s",
			quoteIdent(catalog.SidecarColumn(uint32(c.ID))), quoteIdent(c.Name)))
	}
	return tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.%s AS SELECT %s FROM (%s) m`,
		quoteIdent(into), quoteIdent(t.PhysicalName), strings.Join(cols, ", "), q.SQL), q.Args...)
}

// ApplyMigration runs the same resumable journalled state machine as PostgreSQL
// (§10.4). MySQL is the engine that makes it necessary — DDL commits implicitly,
// so a failed multi-statement migration cannot be rolled back — but both engines
// run it, so failure behaviour is identical and only has to be tested once.
func (a *Adapter) ApplyMigration(ctx context.Context, plan *adapter.MigrationPlan, j adapter.Journal) error {
	if err := j.Begin(ctx, plan); err != nil {
		return fmt.Errorf("journal begin: %w", err)
	}
	done, err := j.Completed(ctx, plan.TableID)
	if err != nil {
		return err
	}
	for _, op := range plan.Ops {
		if done[op.Ordinal] {
			continue
		}
		if err := j.MarkStarted(ctx, plan.TableID, op.Ordinal); err != nil {
			return err
		}
		if a.exec == nil {
			return fmt.Errorf("mysql: adapter has no execution handle; use NewWithExec")
		}
		if err := a.exec(ctx, op.SQL); err != nil {
			return fmt.Errorf("migration op %d (%s): %w", op.Ordinal, op.Kind, err)
		}
		if err := j.MarkComplete(ctx, plan.TableID, op.Ordinal); err != nil {
			return err
		}
	}
	return nil
}
