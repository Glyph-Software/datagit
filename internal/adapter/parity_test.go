package adapter_test

import (
	"strings"
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/mysql"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/core"
)

// The parity gate (PLAN.md W2). Query construction returns SQL plus args rather
// than executing, precisely so the two engines can be compared on identical
// input without a database.
//
// These do not assert the SQL is textually identical — it cannot be, since one
// uses DISTINCT ON and the other ROW_NUMBER(). They assert the STRUCTURAL
// properties that make resolution correct, which must hold on both.

func spec() *adapter.TableSpec {
	return &adapter.TableSpec{
		ID: 1, PhysicalName: "products", Mode: adapter.ModeVersioned,
		Columns: []adapter.Column{
			{ID: 1, Name: "sku", SQLType: "text"},
			{ID: 2, Name: "price", SQLType: "numeric(12,2)", Nullable: true},
			{ID: 3, Name: "category", SQLType: "text", Nullable: true},
		},
		PKColumns: []core.ColID{1},
	}
}

func chain(n int) []adapter.Segment {
	out := make([]adapter.Segment, 0, n)
	for i := 0; i < n; i++ {
		var b [16]byte
		b[15] = byte(i)
		out = append(out, adapter.Segment{BranchID: b, Seq: int64(10 - i)})
	}
	return out
}

type built struct {
	name string
	q    adapter.Query
}

func buildBoth(t *testing.T, s *adapter.ResolveSpec) []built {
	t.Helper()
	pq, err := postgres.New().ResolveQuery(s)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	mq, err := mysql.New().ResolveQuery(s)
	if err != nil {
		t.Fatalf("mysql: %v", err)
	}
	return []built{{"postgres", pq}, {"mysql", mq}}
}

// TestTombstoneFilterIsOutsideTheArms is the §7.3 hazard that resurfaces deleted
// rows. Neither engine may filter op inside an arm's WHERE.
func TestTombstoneFilterIsOutsideTheArms(t *testing.T) {
	for _, b := range buildBoth(t, &adapter.ResolveSpec{Table: spec(), Chain: chain(3)}) {
		t.Run(b.name, func(t *testing.T) {
			// Split at the outer scope: everything before the final WHERE is the
			// arms plus the window/distinct wrapper.
			i := strings.LastIndex(b.q.SQL, "WHERE")
			if i < 0 {
				t.Fatal("no outer WHERE")
			}
			arms, outer := b.q.SQL[:i], b.q.SQL[i:]
			if !strings.Contains(outer, "op <> 3") {
				t.Error("the tombstone filter is not in the outer scope")
			}
			if strings.Contains(arms, "op <> 3") {
				t.Error("a tombstone filter appears inside the union arms: a branch-level " +
					"delete would fall through and the parent's row would resurface (§7.3)")
			}
		})
	}
}

// TestValueFilterUsesTwoPasses is the other §7.3 hazard: a predicate applied
// inside an arm resurfaces a row the branch edited out of range.
func TestValueFilterUsesTwoPasses(t *testing.T) {
	s := &adapter.ResolveSpec{
		Table: spec(), Chain: chain(3),
		Filter: adapter.Compare{Col: 3, Op: adapter.Eq, Value: core.Text("outdoor")},
	}
	for _, b := range buildBoth(t, s) {
		t.Run(b.name, func(t *testing.T) {
			if !strings.Contains(b.q.SQL, "cand") {
				t.Error("no candidate-key pass: the filtered read is not two-pass (§7.3)")
			}
			i := strings.LastIndex(b.q.SQL, "WHERE")
			if !strings.Contains(b.q.SQL[i:], "c_3") {
				t.Error("the value filter is not re-applied to the resolved row")
			}
		})
	}
}

// TestKeyFilterIsPushedIntoTheArms: the one safe pushdown, because row identity
// is immutable (finding F6). It must reach every arm, or point reads scan
// everything.
func TestKeyFilterIsPushedIntoTheArms(t *testing.T) {
	s := &adapter.ResolveSpec{
		Table: spec(), Chain: chain(3),
		KeyFilter: adapter.Compare{Col: 1, Op: adapter.Eq, Value: core.Text("TENT-4P")},
	}
	for _, b := range buildBoth(t, s) {
		t.Run(b.name, func(t *testing.T) {
			if n := strings.Count(b.q.SQL, "c_1` = ") + strings.Count(b.q.SQL, `c_1" = `); n < 3 {
				t.Errorf("the key predicate reached %d arms, want 3: without it a point "+
					"read scans every key in every segment", n)
			}
		})
	}
}

// TestPagedArmsAreLimitedIndividually is Phase 0 finding F9. Without a per-arm
// limit the page bounds the output but not the work.
func TestPagedArmsAreLimitedIndividually(t *testing.T) {
	s := &adapter.ResolveSpec{
		Table: spec(), Chain: chain(3), Limit: 100,
		Filter: adapter.Compare{Col: 3, Op: adapter.Eq, Value: core.Text("outdoor")},
	}
	for _, b := range buildBoth(t, s) {
		t.Run(b.name, func(t *testing.T) {
			// One LIMIT per arm, one for the candidate set, one for the result.
			if n := strings.Count(b.q.SQL, "LIMIT"); n < 5 {
				t.Errorf("found %d LIMIT clauses, want at least 5 (three arms, the "+
					"candidate set, and the result): the page must bound the WORK (F9)", n)
			}
		})
	}
}

// TestBothEnginesTakeTheSameArguments: the parity gate depends on the two
// producing equivalent queries from identical input.
func TestBothEnginesTakeTheSameArguments(t *testing.T) {
	s := &adapter.ResolveSpec{Table: spec(), Chain: chain(4)}
	got := buildBoth(t, s)
	if len(got[0].q.Args) != len(got[1].q.Args) {
		t.Errorf("postgres binds %d args, mysql binds %d: the same input must produce "+
			"the same parameters", len(got[0].q.Args), len(got[1].q.Args))
	}
}

// TestChainDepthCapEnforcedOnBothEngines (§18).
func TestChainDepthCapEnforcedOnBothEngines(t *testing.T) {
	s := &adapter.ResolveSpec{Table: spec(), Chain: chain(9)}
	if _, err := postgres.New().ResolveQuery(s); err == nil {
		t.Error("postgres accepted a chain past the depth cap")
	}
	if _, err := mysql.New().ResolveQuery(s); err == nil {
		t.Error("mysql accepted a chain past the depth cap")
	}
}

// TestCapsDeclareRealDifferences: the matrix exists so callers can branch on
// genuine engine differences rather than discovering them.
func TestCapsDeclareRealDifferences(t *testing.T) {
	p, m := postgres.New().Caps(), mysql.New().Caps()
	if !p.TransactionalDDL || m.TransactionalDDL {
		t.Error("transactional DDL: postgres has it, mysql does not")
	}
	if !p.DistinctOn || m.DistinctOn {
		t.Error("DISTINCT ON: postgres has it, mysql does not")
	}
	if !p.TxnScopedAdvisoryLocks || m.TxnScopedAdvisoryLocks {
		t.Error("advisory lock scope: postgres is transaction-scoped, mysql is session-scoped")
	}
	if !p.PartialIndexes || m.PartialIndexes {
		t.Error("partial indexes: postgres has them, mysql does not")
	}
	// ENUM and SET must NOT be mirrorable: their value lists can diverge between
	// branches (§10.5 rule 5).
	if _, ok := m.SupportedTypes["enum"]; ok {
		t.Error("mysql claims to support ENUM; its value list can diverge between branches")
	}
}
