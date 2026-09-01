// Package model is the reference implementation of DataGit's version model.
//
// It exists to be *obviously correct*, not efficient. Every commit stores a
// complete materialized snapshot of the table, so resolving a branch is a map
// lookup rather than an interval query, and merging is a value-by-value
// comparison of three snapshots with no bitmasks anywhere.
//
// That naivety is the point. The real implementation (internal/engine) resolves
// through half-open seq intervals over a priority-ordered segment chain and uses
// changed_cols masks to narrow merge candidates. The two share no algorithm, so
// when the differential harness in test/property finds them agreeing on ten
// million random operation sequences, that is evidence rather than tautology.
//
// PLAN.md W1: this package must never be imported by non-test code.
package model

import (
	"fmt"
	"sort"

	"github.com/Glyph-Software/datagit/internal/core"
)

// Commit holds a FULL snapshot of the table state. This is the naive choice
// that makes the model trustworthy.
type Commit struct {
	ID      string
	Parents []string
	Branch  string
	Seq     int64
	State   core.Table
}

type Branch struct {
	Name       string
	Head       string
	Parent     string // "" for the root branch
	ForkCommit string // the parent commit this branch's state is relative to
}

type Session struct {
	ID     string
	Branch string
	Base   string
	// Overlay is the session's own view: the full table as the session sees it.
	// Naive again — the engine stores only staged divergence.
	Overlay core.Table
	Open    bool
}

type Repo struct {
	Schema   core.Schema
	commits  map[string]*Commit
	branches map[string]*Branch
	sessions map[string]*Session
	nextID   int
	nextSess int
}

// MergeResult is what a three-way merge produced. When Clean is false the merge
// did not apply and Result is the partial state with conflicted keys omitted
// (DESIGN.md §9.4: conflicts are surfaced, never guessed).
type MergeResult struct {
	Base      string
	Result    core.Table
	Conflicts []core.Conflict
	Clean     bool
	Commit    string // set only when the merge was clean and applied
}

const RootBranch = "main"

func New(schema core.Schema) *Repo {
	r := &Repo{
		Schema:   schema,
		commits:  map[string]*Commit{},
		branches: map[string]*Branch{},
		sessions: map[string]*Session{},
	}
	root := r.newCommitID()
	r.commits[root] = &Commit{ID: root, Branch: RootBranch, Seq: 0, State: core.Table{}}
	r.branches[RootBranch] = &Branch{Name: RootBranch, Head: root}
	return r
}

// newCommitID uses a deterministic counter rather than a content hash. The hash
// chain is M1.5's problem; here the harness needs the model and the engine to
// name the same commit so their merge bases can be compared directly.
func (r *Repo) newCommitID() string {
	r.nextID++
	return fmt.Sprintf("c%d", r.nextID)
}

func (r *Repo) Branch(name string) (*Branch, bool) { b, ok := r.branches[name]; return b, ok }

func (r *Repo) Branches() []string {
	out := make([]string, 0, len(r.branches))
	for n := range r.branches {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *Repo) Head(branch string) string {
	if b, ok := r.branches[branch]; ok {
		return b.Head
	}
	return ""
}

// Resolve returns the branch's current state. Trivial by construction.
func (r *Repo) Resolve(branch string) core.Table {
	b, ok := r.branches[branch]
	if !ok {
		return nil
	}
	return r.commits[b.Head].State.Clone()
}

func (r *Repo) ResolveAt(commit string) core.Table {
	c, ok := r.commits[commit]
	if !ok {
		return nil
	}
	return c.State.Clone()
}

// Apply produces the next state from a change set. Deletes remove the key
// outright — the model has no tombstones, which is exactly why it is a good
// check on the engine, where a mishandled tombstone resurfaces an inherited row
// (DESIGN.md §7.3).
func (r *Repo) apply(state core.Table, cs core.ChangeSet) core.Table {
	next := state.Clone()
	for _, ch := range cs.Sorted() {
		switch ch.Op {
		case core.OpDelete:
			delete(next, ch.PK)
		default:
			next[ch.PK] = ch.Row.Clone()
		}
	}
	return next
}

func (r *Repo) Commit(branch string, cs core.ChangeSet) (string, error) {
	b, ok := r.branches[branch]
	if !ok {
		return "", fmt.Errorf("model: no branch %q", branch)
	}
	parent := r.commits[b.Head]
	id := r.newCommitID()
	r.commits[id] = &Commit{
		ID:      id,
		Parents: []string{b.Head},
		Branch:  branch,
		Seq:     parent.Seq + 1,
		State:   r.apply(parent.State, cs),
	}
	b.Head = id
	return id, nil
}

func (r *Repo) CreateBranch(name, from string) error {
	if _, exists := r.branches[name]; exists {
		return fmt.Errorf("model: branch %q exists", name)
	}
	p, ok := r.branches[from]
	if !ok {
		return fmt.Errorf("model: no parent branch %q", from)
	}
	r.branches[name] = &Branch{Name: name, Head: p.Head, Parent: from, ForkCommit: p.Head}
	return nil
}

// ancestors walks the commit DAG naively, collecting everything reachable.
func (r *Repo) ancestors(commit string) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		if c, ok := r.commits[id]; ok {
			for _, p := range c.Parents {
				walk(p)
			}
		}
	}
	walk(commit)
	return seen
}

// MergeBase computes lowest common ancestors the slow, obvious way: intersect
// the two ancestor sets, then drop any candidate that is itself an ancestor of
// another candidate. The engine uses bidirectional BFS instead.
//
// Returning more than one base is not an error here — DESIGN.md §9.1 refuses the
// merge and names the candidates, and the harness checks both sides agree on the
// candidate set.
func (r *Repo) MergeBase(a, b string) []string {
	aa, ba := r.ancestors(a), r.ancestors(b)
	var common []string
	for id := range aa {
		if ba[id] {
			common = append(common, id)
		}
	}
	var bases []string
	for _, c := range common {
		dominated := false
		for _, o := range common {
			if o == c {
				continue
			}
			if r.ancestors(o)[c] {
				dominated = true
				break
			}
		}
		if !dominated {
			bases = append(bases, c)
		}
	}
	sort.Strings(bases)
	return bases
}

// sameOpt compares two optional rows. Two absent rows are equal; that is what
// makes delete/delete a clean merge.
func sameOpt(a core.Row, aOK bool, b core.Row, bOK bool) bool {
	if aOK != bOK {
		return false
	}
	if !aOK {
		return true
	}
	return a.Equal(b)
}

// mergeTables is the three-way merge, by value only.
//
// Deliberately no masks: for every key it compares the base, ours, and theirs
// images cell by cell across the whole schema. This is DESIGN.md §9.2's case
// table transcribed as directly as possible.
func (r *Repo) mergeTables(base, ours, theirs core.Table) (core.Table, []core.Conflict) {
	keys := map[core.PK]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range ours {
		keys[k] = true
	}
	for k := range theirs {
		keys[k] = true
	}
	sorted := make([]core.PK, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := core.Table{}
	var conflicts []core.Conflict

	for _, pk := range sorted {
		b, bOK := base[pk]
		a, aOK := ours[pk]
		t, tOK := theirs[pk]

		aChanged := !sameOpt(b, bOK, a, aOK)
		tChanged := !sameOpt(b, bOK, t, tOK)

		switch {
		case !aChanged && !tChanged:
			if bOK {
				out[pk] = b.Clone()
			}
		case !aChanged: // only theirs moved
			if tOK {
				out[pk] = t.Clone()
			}
		case !tChanged: // only ours moved
			if aOK {
				out[pk] = a.Clone()
			}
		case sameOpt(a, aOK, t, tOK): // identical change, including delete/delete
			if aOK {
				out[pk] = a.Clone()
			}
		case !bOK: // add/add with different content
			conflicts = append(conflicts, core.Conflict{PK: pk, Kind: core.ConflictAddAdd})
		case !aOK || !tOK: // one deleted, the other modified — never guessed
			conflicts = append(conflicts, core.Conflict{PK: pk, Kind: core.ConflictDeleteModify})
		default:
			merged, cc := r.mergeCells(pk, b, a, t)
			if len(cc) > 0 {
				conflicts = append(conflicts, cc...)
			} else {
				out[pk] = merged
			}
		}
	}
	core.SortConflicts(conflicts)
	return out, conflicts
}

// mergeCells walks every column in the schema. No mask narrowing.
func (r *Repo) mergeCells(pk core.PK, b, a, t core.Row) (core.Row, []core.Conflict) {
	merged := core.Row{}
	var conflicts []core.Conflict
	for _, c := range r.Schema.Cols {
		bv, av, tv := b.Get(c), a.Get(c), t.Get(c)
		switch {
		case av.Equal(bv):
			merged[c] = tv
		case tv.Equal(bv):
			merged[c] = av
		case av.Equal(tv):
			merged[c] = av
		default:
			conflicts = append(conflicts, core.Conflict{
				PK: pk, Kind: core.ConflictCell, Col: c, HasCol: true,
				Base: bv, Ours: av, Theirs: tv,
			})
		}
	}
	return merged, conflicts
}

// Merge merges `from` into `into`. Ours is the target (into), theirs is the
// source (from) — the same orientation Git uses.
func (r *Repo) Merge(from, into string) (MergeResult, error) {
	fb, ok := r.branches[from]
	if !ok {
		return MergeResult{}, fmt.Errorf("model: no branch %q", from)
	}
	tb, ok := r.branches[into]
	if !ok {
		return MergeResult{}, fmt.Errorf("model: no branch %q", into)
	}
	bases := r.MergeBase(tb.Head, fb.Head)
	if len(bases) != 1 {
		return MergeResult{}, fmt.Errorf("model: %d merge bases", len(bases))
	}
	base := bases[0]
	result, conflicts := r.mergeTables(
		r.commits[base].State,
		r.commits[tb.Head].State,
		r.commits[fb.Head].State,
	)
	mr := MergeResult{Base: base, Result: result, Conflicts: conflicts, Clean: len(conflicts) == 0}
	if mr.Clean {
		id := r.newCommitID()
		r.commits[id] = &Commit{
			ID:      id,
			Parents: []string{tb.Head, fb.Head},
			Branch:  into,
			Seq:     r.commits[tb.Head].Seq + 1,
			State:   result.Clone(),
		}
		tb.Head = id
		mr.Commit = id
	}
	return mr, nil
}

// UpdateFromParent implements DESIGN.md §9.6: three-way merge with the branch as
// target and the parent's head as source, then advance the fork point so the
// segment chain stays a tree.
//
// In the model the fork point is only bookkeeping — snapshots do not fall
// through to anything. It is maintained anyway so the harness can assert the
// engine's fork point agrees.
func (r *Repo) UpdateFromParent(branch string) (MergeResult, error) {
	b, ok := r.branches[branch]
	if !ok {
		return MergeResult{}, fmt.Errorf("model: no branch %q", branch)
	}
	if b.Parent == "" {
		return MergeResult{}, fmt.Errorf("model: branch %q has no parent", branch)
	}
	p := r.branches[b.Parent]
	if p.Head == b.ForkCommit {
		return MergeResult{Base: b.ForkCommit, Result: r.commits[b.Head].State.Clone(), Clean: true}, nil
	}
	base := b.ForkCommit
	result, conflicts := r.mergeTables(
		r.commits[base].State,
		r.commits[b.Head].State,
		r.commits[p.Head].State,
	)
	mr := MergeResult{Base: base, Result: result, Conflicts: conflicts, Clean: len(conflicts) == 0}
	if mr.Clean {
		id := r.newCommitID()
		r.commits[id] = &Commit{
			ID:      id,
			Parents: []string{b.Head, p.Head},
			Branch:  branch,
			Seq:     r.commits[b.Head].Seq + 1,
			State:   result.Clone(),
		}
		b.Head = id
		b.ForkCommit = p.Head // §9.6 step 4
		mr.Commit = id
	}
	return mr, nil
}

func (r *Repo) ForkCommit(branch string) string {
	if b, ok := r.branches[branch]; ok {
		return b.ForkCommit
	}
	return ""
}

// --- Sessions (DESIGN.md §6.2). Never available on the root branch. ---

func (r *Repo) OpenSession(branch string) (string, error) {
	b, ok := r.branches[branch]
	if !ok {
		return "", fmt.Errorf("model: no branch %q", branch)
	}
	if branch == RootBranch {
		return "", fmt.Errorf("model: sessions are not permitted on %s", RootBranch)
	}
	r.nextSess++
	id := fmt.Sprintf("s%d", r.nextSess)
	r.sessions[id] = &Session{
		ID: id, Branch: branch, Base: b.Head,
		Overlay: r.commits[b.Head].State.Clone(), Open: true,
	}
	return id, nil
}

func (r *Repo) SessionWrite(sid string, cs core.ChangeSet) error {
	s, ok := r.sessions[sid]
	if !ok || !s.Open {
		return fmt.Errorf("model: no open session %q", sid)
	}
	s.Overlay = r.apply(s.Overlay, cs)
	return nil
}

// SessionResolve is the session's private view: the branch plus its own staged
// work. Nobody else can see it (invariant 9).
func (r *Repo) SessionResolve(sid string) core.Table {
	s, ok := r.sessions[sid]
	if !ok || !s.Open {
		return nil
	}
	return s.Overlay.Clone()
}

// CommitSession publishes the session. It fails if the branch moved underneath,
// mirroring the expected_head check in DESIGN.md §6.2 step 5.
func (r *Repo) CommitSession(sid string) (string, error) {
	s, ok := r.sessions[sid]
	if !ok || !s.Open {
		return "", fmt.Errorf("model: no open session %q", sid)
	}
	b := r.branches[s.Branch]
	if b.Head != s.Base {
		return "", fmt.Errorf("model: branch moved since session opened")
	}
	id := r.newCommitID()
	r.commits[id] = &Commit{
		ID: id, Parents: []string{b.Head}, Branch: s.Branch,
		Seq: r.commits[b.Head].Seq + 1, State: s.Overlay.Clone(),
	}
	b.Head = id
	s.Open = false
	return id, nil
}

func (r *Repo) AbandonSession(sid string) {
	if s, ok := r.sessions[sid]; ok {
		s.Open = false
	}
}

// Diff compares two snapshots directly.
func (r *Repo) Diff(fromCommit, toCommit string) core.ChangeSet {
	from, to := r.ResolveAt(fromCommit), r.ResolveAt(toCommit)
	out := core.ChangeSet{}
	for pk, fr := range from {
		tr, ok := to[pk]
		if !ok {
			out[pk] = core.Change{PK: pk, Op: core.OpDelete}
		} else if !fr.Equal(tr) {
			out[pk] = core.Change{PK: pk, Op: core.OpUpdate, Row: tr.Clone(),
				Changed: core.MaskOf(fr, tr, r.Schema.Cols)}
		}
	}
	for pk, tr := range to {
		if _, ok := from[pk]; !ok {
			out[pk] = core.Change{PK: pk, Op: core.OpInsert, Row: tr.Clone(),
				Changed: core.MaskOf(nil, tr, r.Schema.Cols)}
		}
	}
	return out
}

// ResolveFiltered filters the fully resolved table. Naive by construction: it
// resolves everything and then applies the predicate, which is the definition
// the engine's two-pass form must match (property invariant 10).
func (r *Repo) ResolveFiltered(branch string, pred func(core.Row) bool) core.Table {
	out := core.Table{}
	for pk, row := range r.Resolve(branch) {
		if pred(row) {
			out[pk] = row.Clone()
		}
	}
	return out
}

// PlanUpdateWhere is the reference for a predicate update: filter the full
// resolution, then assign.
func (r *Repo) PlanUpdateWhere(
	branch string,
	pred func(core.Row) bool,
	assign func(core.Row) core.Row,
) core.ChangeSet {
	cs := core.ChangeSet{}
	for pk, row := range r.ResolveFiltered(branch, pred) {
		next := assign(row.Clone())
		if !next.Equal(row) {
			cs[pk] = core.Change{PK: pk, Op: core.OpUpdate, Row: next}
		}
	}
	return cs
}
