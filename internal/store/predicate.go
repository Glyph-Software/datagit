package store

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
)

// Assignment is one column's new value in a predicate update (§7.4).
//
// The expression grammar is deliberately closed: constants, column references,
// arithmetic, concatenation, COALESCE, and CASE. Nothing else — no subqueries,
// no joins, no aggregates, no arbitrary functions. That is what keeps
// UpdateWhere a bounded, single-table operation rather than a query engine.
type Assignment struct {
	Col  core.ColID
	Expr AssignExpr
}

// AssignExpr is a node of the assignment grammar.
type AssignExpr interface{ isAssign() }

type (
	// Const is a literal.
	Const struct{ Value core.Value }
	// ColRef is the row's current value for a column.
	ColRef struct{ Col core.ColID }
	// Arith is `left <op> right` over numerics.
	Arith struct {
		Left  AssignExpr
		Op    ArithOp
		Right AssignExpr
	}
	// Concat joins text.
	Concat struct{ Parts []AssignExpr }
	// Coalesce returns the first non-null.
	Coalesce struct{ Terms []AssignExpr }
	// Case is a guarded choice.
	Case struct {
		When []CaseArm
		Else AssignExpr
	}
)

type CaseArm struct {
	Cond adapter.Expr
	Then AssignExpr
}

func (Const) isAssign()    {}
func (ColRef) isAssign()   {}
func (Arith) isAssign()    {}
func (Concat) isAssign()   {}
func (Coalesce) isAssign() {}
func (Case) isAssign()     {}

type ArithOp uint8

const (
	Add ArithOp = iota + 1
	Sub
	Mul
	Div
)

// PlanUpdateWhere builds the change set a predicate update would produce (§7.4).
//
// It resolves matching rows through the two-pass form, applies the assignments
// to each RESOLVED row, and emits one per-key change per match. The result is
// exactly the change set the equivalent list of per-key updates would produce —
// which is what makes "raise every outdoor price by 8%" one operation rather
// than a query engine.
func (s *Store) PlanUpdateWhere(ctx context.Context, repo *Repo, t *Table, branch string,
	filter adapter.Expr, assigns []Assignment) ([]Change, error) {

	rows, err := s.Read(ctx, repo, t, ReadOptions{Branch: branch, Filter: filter})
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, r := range rows {
		next := r.Clone()
		for _, a := range assigns {
			v, err := evalAssign(a.Expr, r, t)
			if err != nil {
				return nil, fmt.Errorf("assignment to column %d: %w", a.Col, err)
			}
			next[a.Col] = v
		}
		if next.Equal(r) {
			continue // a no-op is not a change
		}
		out = append(out, Change{PK: core.MakePK(r, t.PKColumns), Op: core.OpUpdate, Row: next})
	}
	return out, nil
}

// PlanDeleteWhere builds the change set a predicate delete would produce.
func (s *Store) PlanDeleteWhere(ctx context.Context, repo *Repo, t *Table, branch string,
	filter adapter.Expr) ([]Change, error) {
	rows, err := s.Read(ctx, repo, t, ReadOptions{Branch: branch, Filter: filter})
	if err != nil {
		return nil, err
	}
	out := make([]Change, 0, len(rows))
	for _, r := range rows {
		out = append(out, Change{PK: core.MakePK(r, t.PKColumns), Op: core.OpDelete})
	}
	return out, nil
}

// evalAssign evaluates an assignment expression against a resolved row.
//
// Evaluated in Go, not pushed into SQL, so the semantics are the canonical
// ones: numerics stay exact, and the result is a core.Value that hashes the
// same way whatever the engine underneath.
func evalAssign(e AssignExpr, row core.Row, t *Table) (core.Value, error) {
	switch n := e.(type) {
	case Const:
		return n.Value, nil
	case ColRef:
		return row.Get(n.Col), nil
	case Arith:
		l, err := evalAssign(n.Left, row, t)
		if err != nil {
			return core.Value{}, err
		}
		r, err := evalAssign(n.Right, row, t)
		if err != nil {
			return core.Value{}, err
		}
		return arith(l, n.Op, r)
	case Concat:
		var b strings.Builder
		for _, p := range n.Parts {
			v, err := evalAssign(p, row, t)
			if err != nil {
				return core.Value{}, err
			}
			if v.IsNull() {
				return core.Null(), nil // NULL propagates, as in SQL
			}
			b.WriteString(plainString(v))
		}
		return core.Text(b.String()), nil
	case Coalesce:
		for _, term := range n.Terms {
			v, err := evalAssign(term, row, t)
			if err != nil {
				return core.Value{}, err
			}
			if !v.IsNull() {
				return v, nil
			}
		}
		return core.Null(), nil
	case Case:
		for _, arm := range n.When {
			ok, err := evalPredicate(arm.Cond, row)
			if err != nil {
				return core.Value{}, err
			}
			if ok {
				return evalAssign(arm.Then, row, t)
			}
		}
		if n.Else != nil {
			return evalAssign(n.Else, row, t)
		}
		return core.Null(), nil
	}
	return core.Value{}, fmt.Errorf("unsupported assignment expression %T", e)
}

// arith evaluates numeric arithmetic exactly.
//
// Numerics go through big.Rat rather than float64: a price computed as
// price * 1.08 must be exact, because the result is hashed into history and a
// rounding difference would make the same logical change produce a different
// commit id on a different machine.
func arith(l core.Value, op ArithOp, r core.Value) (core.Value, error) {
	if l.IsNull() || r.IsNull() {
		return core.Null(), nil
	}
	if l.Kind == core.KindInt && r.Kind == core.KindInt {
		a, b := l.Int, r.Int
		switch op {
		case Add:
			return core.Int(a + b), nil
		case Sub:
			return core.Int(a - b), nil
		case Mul:
			return core.Int(a * b), nil
		case Div:
			if b == 0 {
				return core.Null(), nil
			}
			return core.Int(a / b), nil
		}
	}
	lr, err := toRat(l)
	if err != nil {
		return core.Value{}, err
	}
	rr, err := toRat(r)
	if err != nil {
		return core.Value{}, err
	}
	out := newRat()
	switch op {
	case Add:
		out.Add(lr, rr)
	case Sub:
		out.Sub(lr, rr)
	case Mul:
		out.Mul(lr, rr)
	case Div:
		if rr.Sign() == 0 {
			return core.Null(), nil
		}
		out.Quo(lr, rr)
	default:
		return core.Value{}, fmt.Errorf("unsupported arithmetic operator %d", op)
	}
	// Render with enough precision to be exact for terminating decimals, then
	// normalize through the canonical form.
	return core.Numeric(ratString(out))
}

func evalPredicate(e adapter.Expr, row core.Row) (bool, error) {
	switch n := e.(type) {
	case adapter.Compare:
		got := row.Get(n.Col)
		switch n.Op {
		case adapter.Eq:
			return got.Equal(n.Value), nil
		case adapter.Ne:
			return !got.Equal(n.Value), nil
		}
		c, err := compareValues(got, n.Value)
		if err != nil {
			return false, err
		}
		switch n.Op {
		case adapter.Lt:
			return c < 0, nil
		case adapter.Le:
			return c <= 0, nil
		case adapter.Gt:
			return c > 0, nil
		case adapter.Ge:
			return c >= 0, nil
		}
		return false, fmt.Errorf("unsupported comparison in CASE")
	case adapter.IsNull:
		return row.Get(n.Col).IsNull(), nil
	case adapter.In:
		got := row.Get(n.Col)
		for _, v := range n.Values {
			if got.Equal(v) {
				return true, nil
			}
		}
		return false, nil
	case adapter.And:
		for _, term := range n.Terms {
			ok, err := evalPredicate(term, row)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case adapter.Or:
		for _, term := range n.Terms {
			ok, err := evalPredicate(term, row)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case adapter.Not:
		ok, err := evalPredicate(n.Term, row)
		return !ok, err
	}
	return false, fmt.Errorf("unsupported predicate %T in CASE", e)
}

func plainString(v core.Value) string {
	if v.Kind == core.KindText {
		return v.Text
	}
	return v.String()
}

// --- Constraint validation (M3.3, §9.3) ---

// ConstraintViolation is a merge result the database would reject.
type ConstraintViolation struct {
	Kind    string // unique | fk | check | not_null
	Detail  string
	PKs     []core.PK
	Message string
}

// ValidateMerge checks a merged change set against the target table's real
// constraints BEFORE applying it (§9.3).
//
// Dolt lets a merge land and records violations for later resolution. DataGit
// cannot: merging into the default branch writes the live table, which carries
// the application's real constraints, so the database would reject it mid-merge
// with a partial result. Validation therefore happens first, and only a fully
// clean, fully validated merge is applied.
//
// Foreign keys to NON-tracked tables are explicitly not validated here — DataGit
// has no history for the referenced side and cannot know whether the target
// existed at any given commit. Those are caught by the database at apply time,
// which rolls the whole transaction back; this is the sharp edge §9.3 names.
func (s *Store) ValidateMerge(ctx context.Context, repo *Repo, t *Table,
	branch string, changes []Change) ([]ConstraintViolation, error) {

	// Build the post-merge state for the affected keys.
	current, err := s.resolveMap(ctx, s.pool.Direct(), t,
		mustChain(ctx, s, repo, branch), nil)
	if err != nil {
		return nil, err
	}
	for _, ch := range changes {
		if ch.Op == core.OpDelete {
			delete(current, ch.PK)
		} else {
			current[ch.PK] = ch.Row
		}
	}

	var out []ConstraintViolation

	// NOT NULL on the live table.
	for _, c := range t.Columns {
		if c.Nullable {
			continue
		}
		var bad []core.PK
		for pk, row := range current {
			if row.Get(c.ID).IsNull() {
				bad = append(bad, pk)
			}
		}
		if len(bad) > 0 {
			out = append(out, ConstraintViolation{
				Kind: "not_null", Detail: c.Name, PKs: bad,
				Message: fmt.Sprintf("%d row(s) would leave %s null, which the table forbids",
					len(bad), c.Name),
			})
		}
	}

	// Unique indexes on the live table, excluding the primary key, which the
	// keyed model guarantees by construction.
	uniques, err := s.uniqueIndexes(ctx, t)
	if err != nil {
		return nil, err
	}
	for _, u := range uniques {
		seen := map[string][]core.PK{}
		for pk, row := range current {
			var b strings.Builder
			null := false
			for _, id := range u.Cols {
				v := row.Get(id)
				if v.IsNull() {
					null = true // SQL uniqueness ignores nulls
					break
				}
				b.Write(v.MustEncode(nil))
				b.WriteByte('|')
			}
			if null {
				continue
			}
			seen[b.String()] = append(seen[b.String()], pk)
		}
		for _, pks := range seen {
			if len(pks) > 1 {
				out = append(out, ConstraintViolation{
					Kind: "unique", Detail: u.Name, PKs: pks,
					Message: fmt.Sprintf("%d rows would share a value under unique index %s",
						len(pks), u.Name),
				})
			}
		}
	}
	return out, nil
}

type uniqueIndex struct {
	Name string
	Cols []core.ColID
}

func (s *Store) uniqueIndexes(ctx context.Context, t *Table) ([]uniqueIndex, error) {
	idx, err := s.ad.UniqueIndexes(ctx, s.pool.Direct(), t.Physical)
	if err != nil {
		return nil, err
	}
	byName := map[string]core.ColID{}
	for _, c := range t.Columns {
		byName[c.Name] = c.ID
	}
	out := make([]uniqueIndex, 0, len(idx))
	for _, i := range idx {
		u := uniqueIndex{Name: i.Name}
		for _, n := range i.Cols {
			if id, ok := byName[n]; ok {
				u.Cols = append(u.Cols, id)
			}
		}
		out = append(out, u)
	}
	return out, nil
}

func mustChain(ctx context.Context, s *Store, repo *Repo, branch string) []adapter.Segment {
	_, _, _, chain, err := s.loadRef(ctx, s.pool.Direct(), repo, branch)
	if err != nil {
		return nil
	}
	return chain
}

var _ = catalog.SidecarTable

// --- exact decimal helpers ---

func newRat() *big.Rat { return new(big.Rat) }

func toRat(v core.Value) (*big.Rat, error) {
	switch v.Kind {
	case core.KindInt:
		return new(big.Rat).SetInt64(v.Int), nil
	case core.KindNumeric:
		r, ok := new(big.Rat).SetString(v.Text)
		if !ok {
			return nil, fmt.Errorf("%q is not a number", v.Text)
		}
		return r, nil
	case core.KindFloat:
		r := new(big.Rat)
		if _, ok := r.SetString(strconv.FormatFloat(v.Float, 'f', -1, 64)); !ok {
			return nil, fmt.Errorf("cannot convert %v", v.Float)
		}
		return r, nil
	}
	return nil, fmt.Errorf("cannot do arithmetic on %s", v.Kind)
}

// ratString renders a rational exactly when it terminates, and to a bounded
// precision when it does not (1/3, say). Bounding is necessary: a
// non-terminating result has no exact decimal form, and history needs a
// definite value to hash.
const ratPrecision = 12

func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	return strings.TrimRight(strings.TrimRight(r.FloatString(ratPrecision), "0"), ".")
}

// compareValues orders two values of the same kind.
func compareValues(a, b core.Value) (int, error) {
	if a.IsNull() || b.IsNull() {
		return 0, nil
	}
	switch {
	case a.Kind == core.KindInt && b.Kind == core.KindInt:
		switch {
		case a.Int < b.Int:
			return -1, nil
		case a.Int > b.Int:
			return 1, nil
		}
		return 0, nil
	case a.Kind == core.KindText && b.Kind == core.KindText:
		return strings.Compare(a.Text, b.Text), nil
	}
	ar, err := toRat(a)
	if err != nil {
		return 0, err
	}
	br, err := toRat(b)
	if err != nil {
		return 0, err
	}
	return ar.Cmp(br), nil
}
