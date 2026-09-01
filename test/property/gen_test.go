package property

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/Glyph-Software/datagit/internal/core"
)

// The fuzz domain is deliberately tiny: 8 keys, 3 value columns, values 0..4.
//
// Small domains are what make the interesting cases actually occur. With five
// possible values, two branches frequently pick the same one (testing "same
// cell, same value → clean") and a branch frequently sets a column back to the
// value it started with (testing that a set changed_cols bit does not imply a
// changed value — the mask-superset case in core.ColMask). A wide domain would
// make both vanishingly rare and the fuzzing far weaker.
const (
	numKeys   = 8
	valDomain = 5
)

var (
	pkCol   = core.ColID(1)
	valCols = []core.ColID{2, 3, 4}
	schema  = core.Schema{
		Cols:   []core.ColID{1, 2, 3, 4},
		PKCols: []core.ColID{1},
	}
)

func keyName(i int) string { return fmt.Sprintf("k%d", i%numKeys) }

func pkOf(i int) core.PK {
	return core.MakePK(core.Row{pkCol: core.Text(keyName(i))}, schema.PKCols)
}

type OpKind uint8

const (
	KCommit OpKind = iota
	KBranch
	KOpenSession
	KSessionWrite
	KCommitSession
	KAbandonSession
	KMerge
	KUpdateFromParent
	KUpdateWhere
	KLast
)

func (k OpKind) String() string {
	return [...]string{
		"commit", "branch", "open-session", "session-write",
		"commit-session", "abandon-session", "merge", "update-from-parent",
		"update-where",
	}[k]
}

// RowOp is one row's worth of a generated change. Vals[i] < 0 means "leave this
// column as it is", which is what produces the disjoint-cell edits that
// cell-level merge exists for.
type RowOp struct {
	Key  int
	Del  bool
	Vals [3]int
}

type Op struct {
	Kind OpKind
	A    int // branch index (or session index) — reduced modulo at apply time
	B    int
	Rows []RowOp
	FCol int // filter column, index into valCols
	FVal int // filter value: match rows where col == FVal
	ACol int // assignment column
	AVal int
	AAdd bool // assignment is `col = col + AVal` rather than `col = AVal`
}

func (o Op) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s{A:%d B:%d", o.Kind, o.A, o.B)
	if len(o.Rows) > 0 {
		b.WriteString(" rows:[")
		for i, r := range o.Rows {
			if i > 0 {
				b.WriteString(" ")
			}
			if r.Del {
				fmt.Fprintf(&b, "-%s", keyName(r.Key))
			} else {
				fmt.Fprintf(&b, "%s=%v", keyName(r.Key), r.Vals)
			}
		}
		b.WriteString("]")
	}
	if o.Kind == KUpdateWhere {
		fmt.Fprintf(&b, " where c%d==%d set c%d%s%d",
			valCols[o.FCol], o.FVal, valCols[o.ACol],
			map[bool]string{true: "+=", false: "="}[o.AAdd], o.AVal)
	}
	b.WriteString("}")
	return b.String()
}

func genRows(rnd *rand.Rand) []RowOp {
	n := 1 + rnd.Intn(3)
	rows := make([]RowOp, n)
	for i := range rows {
		r := RowOp{Key: rnd.Intn(numKeys)}
		// 20% deletes: frequent enough to exercise tombstone fallthrough (§7.3)
		// and delete/modify conflicts (§9.2) without starving the table.
		if rnd.Intn(100) < 20 {
			r.Del = true
		} else {
			for j := range r.Vals {
				if rnd.Intn(100) < 55 {
					r.Vals[j] = rnd.Intn(valDomain)
				} else {
					r.Vals[j] = -1 // leave alone
				}
			}
		}
		rows[i] = r
	}
	return rows
}

// GenSequence produces a random operation sequence. Weights favour commits and
// merges — the operations whose interaction the harness is really testing.
func GenSequence(rnd *rand.Rand, n int) []Op {
	ops := make([]Op, 0, n)
	for i := 0; i < n; i++ {
		var k OpKind
		switch w := rnd.Intn(100); {
		case w < 34:
			k = KCommit
		case w < 44:
			k = KBranch
		case w < 51:
			k = KOpenSession
		case w < 61:
			k = KSessionWrite
		case w < 68:
			k = KCommitSession
		case w < 71:
			k = KAbandonSession
		case w < 85:
			k = KMerge
		case w < 93:
			k = KUpdateFromParent
		default:
			k = KUpdateWhere
		}
		op := Op{
			Kind: k,
			A:    rnd.Intn(6),
			B:    rnd.Intn(6),
			FCol: rnd.Intn(len(valCols)),
			FVal: rnd.Intn(valDomain),
			ACol: rnd.Intn(len(valCols)),
			AVal: rnd.Intn(valDomain),
			AAdd: rnd.Intn(2) == 0,
		}
		if k == KCommit || k == KSessionWrite {
			op.Rows = genRows(rnd)
		}
		ops = append(ops, op)
	}
	return ops
}

// FormatSequence renders a sequence so a failure can be pasted into the seed
// corpus and replayed.
func FormatSequence(ops []Op) string {
	var b strings.Builder
	for i, o := range ops {
		fmt.Fprintf(&b, "  %2d  %s\n", i, o)
	}
	return b.String()
}
