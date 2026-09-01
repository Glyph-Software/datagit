package property

import (
	"fmt"
	"strings"

	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/engine"
	"github.com/Glyph-Software/datagit/internal/model"
)

// oracle drives the reference model and the real engine through an identical
// operation sequence and asserts they never disagree.
//
// Every operation is applied to both, and after every operation the full state
// of every branch and every open session is compared, along with the standing
// invariants from PLAN.md §Verification.
type oracle struct {
	m *model.Repo
	e *engine.Engine

	branches []string
	sessions []sessionRef
	nextName int
}

type sessionRef struct {
	id     string
	branch string
	open   bool
}

func newOracle() *oracle {
	return &oracle{
		m:        model.New(schema),
		e:        engine.New(schema),
		branches: []string{model.RootBranch},
	}
}

func (o *oracle) pick(i int) string { return o.branches[i%len(o.branches)] }

// buildChangeSet turns generated RowOps into a concrete change set, reading the
// current row from the reference view. Both sides receive the identical change
// set, exactly as a client that reads then writes would produce.
func (o *oracle) buildChangeSet(rows []RowOp, view core.Table) core.ChangeSet {
	cs := core.ChangeSet{}
	for _, r := range rows {
		pk := pkOf(r.Key)
		cur, live := view[pk]
		if r.Del {
			if live {
				cs[pk] = core.Change{PK: pk, Op: core.OpDelete}
			}
			continue
		}
		row := core.Row{pkCol: core.Text(keyName(r.Key))}
		for i, c := range valCols {
			switch {
			case r.Vals[i] >= 0:
				row[c] = core.Int(int64(r.Vals[i]))
			case live:
				row[c] = cur.Get(c)
			default:
				row[c] = core.Int(0)
			}
		}
		op := core.OpUpdate
		if !live {
			op = core.OpInsert
		}
		cs[pk] = core.Change{PK: pk, Op: op, Row: row}
	}
	return cs
}

func predicate(col core.ColID, val int) func(core.Row) bool {
	return func(r core.Row) bool { return r.Get(col).Equal(core.Int(int64(val))) }
}

func assignment(col core.ColID, val int, add bool) func(core.Row) core.Row {
	return func(r core.Row) core.Row {
		if add {
			r[col] = core.Int(r.Get(col).Int + int64(val))
		} else {
			r[col] = core.Int(int64(val))
		}
		return r
	}
}

// apply runs one operation on both sides. Any operation that is invalid for the
// current state is skipped identically on both, so the two stay in lockstep.
func (o *oracle) apply(op Op) error {
	switch op.Kind {
	case KCommit:
		b := o.pick(op.A)
		cs := o.buildChangeSet(op.Rows, o.m.Resolve(b))
		if len(cs) == 0 {
			return nil
		}
		mid, merr := o.m.Commit(b, cs)
		eid, eerr := o.e.Commit(b, cs)
		return sameOutcome("commit", mid, eid, merr, eerr)

	case KBranch:
		from := o.pick(op.A)
		o.nextName++
		name := fmt.Sprintf("b%d", o.nextName)
		// §18: branch creation is refused past chain depth 8.
		if o.e.ChainDepth(from) >= 8 {
			return nil
		}
		merr := o.m.CreateBranch(name, from)
		eerr := o.e.CreateBranch(name, from)
		if (merr == nil) != (eerr == nil) {
			return fmt.Errorf("create-branch disagreement: model=%v engine=%v", merr, eerr)
		}
		if merr == nil {
			o.branches = append(o.branches, name)
		}
		return nil

	case KOpenSession:
		b := o.pick(op.A)
		mid, merr := o.m.OpenSession(b)
		eid, eerr := o.e.OpenSession(b)
		if err := sameOutcome("open-session", mid, eid, merr, eerr); err != nil {
			return err
		}
		if merr == nil {
			o.sessions = append(o.sessions, sessionRef{id: mid, branch: b, open: true})
		}
		return nil

	case KSessionWrite:
		s := o.openSession(op.A)
		if s == nil {
			return nil
		}
		cs := o.buildChangeSet(op.Rows, o.m.SessionResolve(s.id))
		if len(cs) == 0 {
			return nil
		}
		merr := o.m.SessionWrite(s.id, cs)
		eerr := o.e.SessionWrite(s.id, cs)
		if (merr == nil) != (eerr == nil) {
			return fmt.Errorf("session-write disagreement: model=%v engine=%v", merr, eerr)
		}
		return nil

	case KCommitSession:
		s := o.openSession(op.A)
		if s == nil {
			return nil
		}
		mid, merr := o.m.CommitSession(s.id)
		eid, eerr := o.e.CommitSession(s.id)
		// A branch that moved since the session opened makes both fail.
		if merr != nil && eerr != nil {
			s.open = false
			o.e.AbandonSession(s.id)
			o.m.AbandonSession(s.id)
			return nil
		}
		if err := sameOutcome("commit-session", mid, eid, merr, eerr); err != nil {
			return err
		}
		s.open = false
		return nil

	case KAbandonSession:
		s := o.openSession(op.A)
		if s == nil {
			return nil
		}
		o.m.AbandonSession(s.id)
		o.e.AbandonSession(s.id)
		s.open = false
		return nil

	case KMerge:
		from, into := o.pick(op.A), o.pick(op.B)
		if from == into {
			return nil
		}
		mr, merr := o.m.Merge(from, into)
		er, eerr := o.e.Merge(from, into)
		if (merr == nil) != (eerr == nil) {
			return fmt.Errorf("merge %s->%s error disagreement: model=%v engine=%v",
				from, into, merr, eerr)
		}
		if merr != nil {
			return nil // both refused, e.g. multiple merge bases (§9.1)
		}
		return compareMerge(fmt.Sprintf("merge %s->%s", from, into), mr, er)

	case KUpdateFromParent:
		b := o.pick(op.A)
		mr, merr := o.m.UpdateFromParent(b)
		er, eerr := o.e.UpdateFromParent(b)
		if (merr == nil) != (eerr == nil) {
			return fmt.Errorf("update-from-parent %s error disagreement: model=%v engine=%v",
				b, merr, eerr)
		}
		if merr != nil {
			return nil
		}
		return compareMerge("update-from-parent "+b, mr, er)

	case KUpdateWhere:
		b := o.pick(op.A)
		pred := predicate(valCols[op.FCol], op.FVal)
		assign := assignment(valCols[op.ACol], op.AVal, op.AAdd)

		// Invariant 12: the predicate update's change set must equal the one the
		// equivalent per-key updates would produce.
		mcs := o.m.PlanUpdateWhere(b, pred, assign)
		ecs := o.e.PlanUpdateWhere(b, pred, assign)
		if err := compareChangeSets("update-where plan on "+b, mcs, ecs); err != nil {
			return err
		}
		if len(mcs) == 0 {
			return nil
		}
		mid, merr := o.m.Commit(b, mcs)
		eid, eerr := o.e.Commit(b, ecs)
		return sameOutcome("update-where commit", mid, eid, merr, eerr)
	}
	return nil
}

func (o *oracle) openSession(i int) *sessionRef {
	var open []*sessionRef
	for idx := range o.sessions {
		if o.sessions[idx].open {
			open = append(open, &o.sessions[idx])
		}
	}
	if len(open) == 0 {
		return nil
	}
	return open[i%len(open)]
}

func sameOutcome(what, mid, eid string, merr, eerr error) error {
	if (merr == nil) != (eerr == nil) {
		return fmt.Errorf("%s error disagreement: model=%v engine=%v", what, merr, eerr)
	}
	if merr == nil && mid != eid {
		return fmt.Errorf("%s commit id disagreement: model=%s engine=%s", what, mid, eid)
	}
	return nil
}

func compareMerge(what string, mr model.MergeResult, er engine.MergeResult) error {
	if mr.Clean != er.Clean {
		return fmt.Errorf("%s: clean disagreement model=%v engine=%v\nmodel conflicts:\n%s\nengine conflicts:\n%s",
			what, mr.Clean, er.Clean, fmtConflicts(mr.Conflicts), fmtConflicts(er.Conflicts))
	}
	if mr.Base != er.Base {
		return fmt.Errorf("%s: merge base disagreement model=%s engine=%s", what, mr.Base, er.Base)
	}
	if !mr.Result.Equal(er.Result) {
		return fmt.Errorf("%s: merged state disagreement\nmodel:\n%s\nengine:\n%s",
			what, fmtTable(mr.Result), fmtTable(er.Result))
	}
	if fmtConflicts(mr.Conflicts) != fmtConflicts(er.Conflicts) {
		return fmt.Errorf("%s: conflict set disagreement\nmodel:\n%s\nengine:\n%s",
			what, fmtConflicts(mr.Conflicts), fmtConflicts(er.Conflicts))
	}
	return nil
}

func compareChangeSets(what string, a, b core.ChangeSet) error {
	if len(a) != len(b) {
		return fmt.Errorf("%s: change set size %d vs %d", what, len(a), len(b))
	}
	for pk, ca := range a {
		cb, ok := b[pk]
		if !ok {
			return fmt.Errorf("%s: engine missing key %s", what, pk)
		}
		if ca.Op != cb.Op || !ca.Row.Equal(cb.Row) {
			return fmt.Errorf("%s: key %s differs: model=%s engine=%s", what, pk, ca, cb)
		}
	}
	return nil
}

// check runs every standing invariant. Numbers refer to PLAN.md §Verification.
func (o *oracle) check() error {
	// Invariants 1 and 2: the resolved state of every branch matches the model,
	// which includes main. Branch activity therefore cannot perturb main.
	for _, b := range o.branches {
		mt, et := o.m.Resolve(b), o.e.Resolve(b)
		if !mt.Equal(et) {
			return fmt.Errorf("branch %s state disagreement\nmodel:\n%s\nengine:\n%s",
				b, fmtTable(mt), fmtTable(et))
		}
		if o.m.Head(b) != o.e.Head(b) {
			return fmt.Errorf("branch %s head disagreement: model=%s engine=%s",
				b, o.m.Head(b), o.e.Head(b))
		}
		// Invariant 11: the fork point tracks identically on both sides.
		if o.m.ForkCommit(b) != o.e.ForkCommit(b) {
			return fmt.Errorf("branch %s fork disagreement: model=%s engine=%s",
				b, o.m.ForkCommit(b), o.e.ForkCommit(b))
		}
		if d := o.e.ChainDepth(b); d > 8 {
			return fmt.Errorf("branch %s chain depth %d exceeds the cap of 8", b, d)
		}
	}

	// Invariant 9: staged rows are visible only to the session that wrote them.
	for _, s := range o.sessions {
		if !s.open {
			continue
		}
		mt, et := o.m.SessionResolve(s.id), o.e.SessionResolve(s.id)
		if !mt.Equal(et) {
			return fmt.Errorf("session %s state disagreement\nmodel:\n%s\nengine:\n%s",
				s.id, fmtTable(mt), fmtTable(et))
		}
	}

	// Invariant 8: no uncommitted state ever exists on the default branch.
	if n := o.e.StagedOnBranch(model.RootBranch); n != 0 {
		return fmt.Errorf("invariant 8: %d staged rows on %s", n, model.RootBranch)
	}
	if n := o.e.ZeroHashOnBranch(model.RootBranch); n != 0 {
		return fmt.Errorf("invariant 8: %d zero-hash rows on %s", n, model.RootBranch)
	}

	// Invariant 4: exactly one open version per key per branch, no overlaps.
	for b, keys := range o.e.OpenVersionsPerKey() {
		for pk, n := range keys {
			if n != 1 {
				return fmt.Errorf("invariant 4: branch %s key %s has %d open versions", b, pk, n)
			}
		}
	}
	if err := o.e.IntervalsSane(); err != nil {
		return fmt.Errorf("invariant 4: %w", err)
	}

	// Invariant 10: a filtered branch read equals the filter applied to the full
	// resolution. This is the two-pass check — the hazard is that pushing the
	// predicate into the resolution arms resurfaces a parent row the branch
	// edited out of range.
	for _, b := range o.branches {
		for _, c := range valCols {
			for v := 0; v < valDomain; v++ {
				pred := predicate(c, v)
				want := o.m.ResolveFiltered(b, pred)
				got := o.e.ResolveFiltered(b, pred)
				if !want.Equal(got) {
					return fmt.Errorf(
						"invariant 10: filtered read on %s (col %d == %d) disagreement\nmodel:\n%s\nengine:\n%s",
						b, c, v, fmtTable(want), fmtTable(got))
				}
			}
		}
	}
	return nil
}

func fmtTable(t core.Table) string {
	if len(t) == 0 {
		return "    (empty)"
	}
	var b strings.Builder
	for _, k := range t.Keys() {
		fmt.Fprintf(&b, "    %s -> %s\n", k, t[k])
	}
	return strings.TrimRight(b.String(), "\n")
}

func fmtConflicts(cs []core.Conflict) string {
	if len(cs) == 0 {
		return "    (none)"
	}
	core.SortConflicts(cs)
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	return strings.TrimRight(b.String(), "\n")
}
