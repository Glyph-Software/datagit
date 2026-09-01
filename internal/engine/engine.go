// Package engine is the real implementation of DataGit's version model: the one
// whose data structures mirror what the SQL schema in DESIGN.md §5.2 actually
// stores, and whose algorithms are the ones the database will run.
//
// Where internal/model keeps a full snapshot per commit, this package keeps a
// flat list of row versions over half-open [seq_from, seq_to) intervals tagged
// with a branch, exactly like datagit_v_<table>. Resolution walks a
// priority-ordered segment chain (§7.3); merge narrows its cell scan with
// changed_cols masks (§9.2).
//
// The in-memory version list stands in for the SQL table so the algorithms can
// be fuzzed against the reference model without a database in the loop. S1
// measures whether the same query shapes hold up in PostgreSQL at scale.
package engine

import (
	"fmt"
	"math"
	"sort"

	"github.com/Glyph-Software/datagit/internal/core"
)

// MaxSeq is the open-interval sentinel. DESIGN.md §5.2d: an explicit sentinel
// rather than NULL, because NULL in a range predicate defeats the index.
const MaxSeq int64 = math.MaxInt64

// Version is one row of datagit_v_<table>.
type Version struct {
	Branch  string
	PK      core.PK
	SeqFrom int64
	SeqTo   int64
	Op      core.Op
	Commit  string
	Session string // non-empty while staged in a session; cleared on commit
	Changed core.ColMask
	Row     core.Row
}

// Segment is one link of a resolution chain: "this branch, as of this seq".
type Segment struct {
	Branch string
	Seq    int64
}

type Branch struct {
	Name       string
	HeadSeq    int64
	HeadCommit string
	Parent     string
	ForkSeq    int64 // the parent's seq this branch currently diverges at
	ForkCommit string

	// Chain is the inherited tail of this branch's resolution chain, excluding
	// the branch itself, CAPTURED AT FORK TIME.
	//
	// PHASE 0 FINDING F1. It must be stored, not recomputed by walking
	// parent.Parent and reading each ancestor's *current* ForkSeq. Ancestors
	// move: UpdateFromParent (§9.6) advances a branch's fork point, and if a
	// descendant rebuilt its chain from live ancestor state, the descendant's
	// inherited view would silently change because its parent absorbed
	// unrelated work. A branch's state must depend only on its own history and
	// the state it forked from.
	Chain []Segment
}

// commitMeta records the resolution chain in force when the commit was made.
//
// PHASE 0 FINDING. The chain must be stored per commit, not derived from the
// branch's current fork point. UpdateFromParent (§9.6) advances the fork point,
// and if historical reads rebuilt the chain from the branch's *current* fork
// point, every commit made before the advance would silently resolve against
// the wrong parent state — time travel would return answers that were never
// true. Storing the chain makes older commits keep their own view.
// See docs/phase0/findings.md F1.
type commitMeta struct {
	ID      string
	Parents []string
	Branch  string
	Seq     int64
	Chain   []Segment
}

type session struct {
	ID      string
	Branch  string
	Base    string
	BaseSeq int64 // the branch seq the session was opened at
	Open    bool
}

type Engine struct {
	Schema   core.Schema
	rows     []Version
	branches map[string]*Branch
	commits  map[string]*commitMeta
	sessions map[string]*session
	nextID   int
	nextSess int
}

type MergeResult struct {
	Base      string
	Result    core.Table
	Conflicts []core.Conflict
	Clean     bool
	Commit    string
}

const RootBranch = "main"

func New(schema core.Schema) *Engine {
	e := &Engine{
		Schema:   schema,
		branches: map[string]*Branch{},
		commits:  map[string]*commitMeta{},
		sessions: map[string]*session{},
	}
	root := e.newCommitID()
	e.branches[RootBranch] = &Branch{Name: RootBranch, HeadSeq: 0, HeadCommit: root}
	e.commits[root] = &commitMeta{
		ID: root, Branch: RootBranch, Seq: 0,
		Chain: []Segment{{Branch: RootBranch, Seq: 0}},
	}
	return e
}

func (e *Engine) newCommitID() string {
	e.nextID++
	return fmt.Sprintf("c%d", e.nextID)
}

func (e *Engine) Head(branch string) string {
	if b, ok := e.branches[branch]; ok {
		return b.HeadCommit
	}
	return ""
}

func (e *Engine) ForkCommit(branch string) string {
	if b, ok := e.branches[branch]; ok {
		return b.ForkCommit
	}
	return ""
}

func (e *Engine) HeadSeq(branch string) int64 {
	if b, ok := e.branches[branch]; ok {
		return b.HeadSeq
	}
	return 0
}

// VersionCount reports the sidecar row count, used by the harness to assert the
// overlay stays proportional to divergence rather than to table size.
func (e *Engine) VersionCount() int { return len(e.rows) }

// --- Resolution (DESIGN.md §7.3) ---

// chainFor builds the priority-ordered chain for a branch at a given seq, index
// 0 highest. It prepends the branch itself to the inherited tail captured at
// fork time — see the note on Branch.Chain (finding F1).
func (e *Engine) chainFor(branch string, atSeq int64) []Segment {
	b, ok := e.branches[branch]
	if !ok {
		return []Segment{{Branch: branch, Seq: atSeq}}
	}
	chain := make([]Segment, 0, len(b.Chain)+1)
	chain = append(chain, Segment{Branch: branch, Seq: atSeq})
	chain = append(chain, b.Chain...)
	return chain
}

// inheritedChainFrom builds the tail a new (or newly updated) branch inherits
// from `parent` as of the parent's current head.
func (e *Engine) inheritedChainFrom(parent string) []Segment {
	p := e.branches[parent]
	tail := make([]Segment, 0, len(p.Chain)+1)
	tail = append(tail, Segment{Branch: parent, Seq: p.HeadSeq})
	tail = append(tail, p.Chain...)
	return tail
}

// winners resolves each primary key to the version from the highest-priority
// segment that has one in interval. Tombstones are NOT filtered here.
func (e *Engine) winners(chain []Segment, sessionID string) map[core.PK]Version {
	best := map[core.PK]Version{}
	bestPrio := map[core.PK]int{}

	consider := func(prio int, v Version) {
		if p, seen := bestPrio[v.PK]; seen && p <= prio {
			return
		}
		best[v.PK] = v
		bestPrio[v.PK] = prio
	}

	// Priority -1: the session's own staged rows, layered over the branch.
	// DESIGN.md §7.3, "Reads inside a session".
	if sessionID != "" {
		for _, v := range e.rows {
			if v.Session == sessionID {
				consider(-1, v)
			}
		}
	}
	for prio, seg := range chain {
		for _, v := range e.rows {
			if v.Session != "" {
				continue // staged rows are invisible outside their session
			}
			if v.Branch != seg.Branch {
				continue
			}
			if v.SeqFrom <= seg.Seq && seg.Seq < v.SeqTo {
				consider(prio, v)
			}
		}
	}
	return best
}

// resolveChain applies the `op != delete` filter OUTSIDE the per-segment scan.
//
// Doing it inside `winners` would let a branch-level delete fall through to the
// parent's older version and resurface a row the branch deleted — DESIGN.md
// §7.3, and property invariant 3.
func (e *Engine) resolveChain(chain []Segment, sessionID string) core.Table {
	out := core.Table{}
	for pk, v := range e.winners(chain, sessionID) {
		if v.Op == core.OpDelete {
			continue
		}
		out[pk] = v.Row.Clone()
	}
	return out
}

// Resolve returns the branch's live state at its head.
func (e *Engine) Resolve(branch string) core.Table {
	b, ok := e.branches[branch]
	if !ok {
		return nil
	}
	return e.resolveChain(e.chainFor(branch, b.HeadSeq), "")
}

// StateAtCommit resolves the table as of an arbitrary commit, using the chain
// recorded on that commit rather than the branch's current one.
func (e *Engine) StateAtCommit(id string) core.Table {
	cm, ok := e.commits[id]
	if !ok {
		return core.Table{}
	}
	return e.resolveChain(cm.Chain, "")
}

// ResolveFiltered is the two-pass form from DESIGN.md §7.3.
//
// Pass 1 collects candidate keys: any key whose in-interval version matches the
// predicate in ANY segment. That is a superset of the answer.
// Pass 2 fully resolves exactly those keys and applies the predicate to the
// winner.
//
// Applying the predicate inside the segment scan instead would drop a row from
// the branch's arm when the branch edited it out of the predicate's range, and
// the parent's still-matching version would resurface. Property invariant 10.
func (e *Engine) ResolveFiltered(branch string, pred func(core.Row) bool) core.Table {
	b, ok := e.branches[branch]
	if !ok {
		return nil
	}
	chain := e.chainFor(branch, b.HeadSeq)

	// Pass 1: candidate keys.
	candidates := map[core.PK]bool{}
	for _, seg := range chain {
		for _, v := range e.rows {
			if v.Session != "" || v.Branch != seg.Branch {
				continue
			}
			if v.SeqFrom <= seg.Seq && seg.Seq < v.SeqTo && v.Op != core.OpDelete && pred(v.Row) {
				candidates[v.PK] = true
			}
		}
	}

	// Pass 2: resolve exactly those keys, then filter the winner.
	out := core.Table{}
	for pk, v := range e.winners(chain, "") {
		if !candidates[pk] || v.Op == core.OpDelete {
			continue
		}
		if pred(v.Row) {
			out[pk] = v.Row.Clone()
		}
	}
	return out
}

// --- Writes (DESIGN.md §6.1, §6.2) ---

func (e *Engine) closeOpen(branch string, pk core.PK, at int64) {
	for i := range e.rows {
		v := &e.rows[i]
		if v.Branch == branch && v.PK == pk && v.Session == "" && v.SeqTo == MaxSeq {
			v.SeqTo = at
			return
		}
	}
}

// recordCommit stores commit metadata with the chain in force right now.
func (e *Engine) recordCommit(id, branch string, seq int64, parents []string) {
	e.commits[id] = &commitMeta{
		ID: id, Parents: parents, Branch: branch, Seq: seq,
		Chain: e.chainFor(branch, seq),
	}
}

// Commit applies a whole change set in one step: there is no staging on the
// default branch (DESIGN.md §6.1). changed_cols is computed here, from the
// resolved current image, not supplied by the caller — mask maintenance is
// precisely what the differential harness needs to exercise.
func (e *Engine) Commit(branch string, cs core.ChangeSet) (string, error) {
	b, ok := e.branches[branch]
	if !ok {
		return "", fmt.Errorf("engine: no branch %q", branch)
	}
	current := e.Resolve(branch)
	newSeq := b.HeadSeq + 1
	id := e.newCommitID()
	parent := b.HeadCommit

	for _, ch := range cs.Sorted() { // PK order, per §6.1 property 3
		before, live := current[ch.PK]
		switch ch.Op {
		case core.OpDelete:
			if !live {
				continue // deleting an absent row is a no-op, not a tombstone
			}
			e.closeOpen(branch, ch.PK, newSeq)
			e.rows = append(e.rows, Version{
				Branch: branch, PK: ch.PK, SeqFrom: newSeq, SeqTo: MaxSeq,
				Op: core.OpDelete, Commit: id,
				Changed: core.MaskOf(before, nil, e.Schema.Cols),
			})
		default:
			op := core.OpUpdate
			if !live {
				op = core.OpInsert
			}
			mask := core.MaskOf(before, ch.Row, e.Schema.Cols)
			e.closeOpen(branch, ch.PK, newSeq)
			e.rows = append(e.rows, Version{
				Branch: branch, PK: ch.PK, SeqFrom: newSeq, SeqTo: MaxSeq,
				Op: op, Commit: id, Changed: mask, Row: ch.Row.Clone(),
			})
		}
	}

	b.HeadSeq = newSeq
	b.HeadCommit = id
	e.recordCommit(id, branch, newSeq, []string{parent})
	return id, nil
}

func (e *Engine) CreateBranch(name, from string) error {
	if _, exists := e.branches[name]; exists {
		return fmt.Errorf("engine: branch %q exists", name)
	}
	p, ok := e.branches[from]
	if !ok {
		return fmt.Errorf("engine: no parent branch %q", from)
	}
	// O(1): a fork point and a captured chain, no data copied. DESIGN.md G4.
	e.branches[name] = &Branch{
		Name: name, HeadSeq: 0, HeadCommit: p.HeadCommit,
		Parent: from, ForkSeq: p.HeadSeq, ForkCommit: p.HeadCommit,
		Chain: e.inheritedChainFrom(from),
	}
	return nil
}

// ChainDepth reports the branch's current segment count, capped at 8 (§18).
func (e *Engine) ChainDepth(branch string) int {
	b, ok := e.branches[branch]
	if !ok {
		return 0
	}
	return len(e.chainFor(branch, b.HeadSeq))
}

// --- Merge base (DESIGN.md §9.1) ---

// MergeBase uses bidirectional breadth-first search over the commit DAG, which
// is what the real implementation does. The model computes full ancestor sets
// and intersects them instead.
func (e *Engine) MergeBase(a, b string) []string {
	reach := func(start string) map[string]int {
		dist := map[string]int{start: 0}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			cm, ok := e.commits[cur]
			if !ok {
				continue
			}
			for _, p := range cm.Parents {
				if _, seen := dist[p]; !seen {
					dist[p] = dist[cur] + 1
					queue = append(queue, p)
				}
			}
		}
		return dist
	}
	da, db := reach(a), reach(b)
	var common []string
	for id := range da {
		if _, ok := db[id]; ok {
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
			if _, ok := reach(o)[c]; ok {
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

// --- Merge (DESIGN.md §9.2) ---

// accumulateMask ORs the changed_cols of every version a branch wrote in
// (fromSeq, toSeq].
func (e *Engine) accumulateMask(branch string, fromSeq, toSeq int64) map[core.PK]core.ColMask {
	out := map[core.PK]core.ColMask{}
	for _, v := range e.rows {
		if v.Branch != branch || v.Session != "" {
			continue
		}
		if v.SeqFrom > fromSeq && v.SeqFrom <= toSeq {
			out[v.PK] = out[v.PK].Or(v.Changed)
		}
	}
	return out
}

// maskSince accumulates every column that may have changed on `branch` since
// `baseCommit`, by diffing the base commit's resolution chain against the
// branch's current one.
//
// PHASE 0 FINDING F4. It is NOT enough to walk branch parentage and stop at the
// base's branch. UpdateFromParent (§9.6) advances a branch's fork point and then
// prunes overlay rows that now match the parent (step 5), so a branch's
// effective state can change with nothing written to its own overlay. A walk
// that stopped at the base's branch would miss every such change, and the merge
// would silently keep the base value for those cells.
//
// Diffing chains is the general form: for every segment, accumulate the range
// between what that segment covered at the base and what it covers now. A
// segment absent from the base chain is new in its entirety.
//
// A branch's local seq and its parent's seq are different sequence spaces and
// are never compared (DESIGN.md §3.3) — each segment is bounded against its own
// branch's seq only.
func (e *Engine) maskSince(branch, baseCommit string) map[core.PK]core.ColMask {
	baseCM, ok := e.commits[baseCommit]
	if !ok {
		return map[core.PK]core.ColMask{}
	}
	baseUpper := make(map[string]int64, len(baseCM.Chain))
	for _, s := range baseCM.Chain {
		baseUpper[s.Branch] = s.Seq
	}

	out := map[core.PK]core.ColMask{}
	add := func(branch string, lo, hi int64) {
		if hi <= lo {
			return
		}
		for pk, m := range e.accumulateMask(branch, lo, hi) {
			out[pk] = out[pk].Or(m)
		}
	}

	seen := map[string]bool{}
	for _, seg := range e.chainFor(branch, e.branches[branch].HeadSeq) {
		seen[seg.Branch] = true
		baseSeq, inBase := baseUpper[seg.Branch]
		if !inBase {
			add(seg.Branch, 0, seg.Seq) // wholly absent from the base's view
			continue
		}
		// PHASE 0 FINDING F5. The range is taken in BOTH directions.
		//
		// A branch differs from the base not only by carrying changes the base
		// lacks, but also by LACKING changes the base carries. That happens
		// routinely: a branch forked before its parent advanced, and a sibling's
		// merge base sits at the parent's later commit. Accumulating only the
		// forward range (seg.Seq > baseSeq) silently misses every column in the
		// backward range, and the merge then keeps the base value for those
		// cells instead of the branch's.
		//
		// Ordering the bounds makes the mask a superset of the symmetric
		// difference between the two views, which is what soundness requires.
		lo, hi := baseSeq, seg.Seq
		if hi < lo {
			lo, hi = hi, lo
		}
		add(seg.Branch, lo, hi)
	}
	// Segments the base could see that this branch cannot see at all.
	for _, s := range baseCM.Chain {
		if !seen[s.Branch] {
			add(s.Branch, 0, s.Seq)
		}
	}
	return out
}

func sameOpt(a core.Row, aOK bool, b core.Row, bOK bool) bool {
	if aOK != bOK {
		return false
	}
	if !aOK {
		return true
	}
	return a.Equal(b)
}

// mergeStates is the three-way merge with mask-narrowed cell comparison.
//
// PHASE 0 FINDING. The masks narrow which columns get examined and nothing more.
// Every decision is still made by comparing values, because a set bit does not
// imply a changed value: a branch that changes a column and changes it back
// leaves the bit set with the value equal to base. DESIGN.md §9.2's case table
// is written in terms of values, so disjoint masks are sufficient for a clean
// merge but overlapping masks are NOT sufficient for a conflict.
// See docs/phase0/findings.md F2.
//
// Columns outside the union of both masks were touched by neither side and
// therefore still hold the base value.
func (e *Engine) mergeStates(
	base, ours, theirs core.Table,
	maskO, maskT map[core.PK]core.ColMask,
) (core.Table, []core.Conflict) {
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
		case !aChanged:
			if tOK {
				out[pk] = t.Clone()
			}
		case !tChanged:
			if aOK {
				out[pk] = a.Clone()
			}
		case sameOpt(a, aOK, t, tOK): // identical change, including delete/delete
			if aOK {
				out[pk] = a.Clone()
			}
		case !bOK:
			conflicts = append(conflicts, core.Conflict{PK: pk, Kind: core.ConflictAddAdd})
		case !aOK || !tOK: // delete/modify — never guessed
			conflicts = append(conflicts, core.Conflict{PK: pk, Kind: core.ConflictDeleteModify})
		default:
			candidates := core.UnionCols(maskO[pk], maskT[pk])
			touched := make(map[core.ColID]bool, len(candidates))
			for _, c := range candidates {
				touched[c] = true
			}
			merged := core.Row{}
			for _, c := range e.Schema.Cols {
				if !touched[c] {
					merged[c] = b.Get(c) // untouched by both sides
				}
			}
			var cc []core.Conflict
			for _, c := range candidates {
				bv, av, tv := b.Get(c), a.Get(c), t.Get(c)
				switch {
				case av.Equal(bv):
					merged[c] = tv
				case tv.Equal(bv):
					merged[c] = av
				case av.Equal(tv):
					merged[c] = av
				default:
					cc = append(cc, core.Conflict{
						PK: pk, Kind: core.ConflictCell, Col: c, HasCol: true,
						Base: bv, Ours: av, Theirs: tv,
					})
				}
			}
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

// applyMergeResult writes a merged table onto a branch as one commit.
func (e *Engine) applyMergeResult(branch string, result core.Table, parents []string) string {
	b := e.branches[branch]
	current := e.Resolve(branch)
	newSeq := b.HeadSeq + 1
	id := e.newCommitID()

	for _, pk := range result.Keys() {
		row := result[pk]
		before, live := current[pk]
		if live && before.Equal(row) {
			continue
		}
		op := core.OpUpdate
		if !live {
			op = core.OpInsert
		}
		e.closeOpen(branch, pk, newSeq)
		e.rows = append(e.rows, Version{
			Branch: branch, PK: pk, SeqFrom: newSeq, SeqTo: MaxSeq,
			Op: op, Commit: id, Changed: core.MaskOf(before, row, e.Schema.Cols),
			Row: row.Clone(),
		})
	}
	for _, pk := range current.Keys() {
		if _, still := result[pk]; still {
			continue
		}
		e.closeOpen(branch, pk, newSeq)
		e.rows = append(e.rows, Version{
			Branch: branch, PK: pk, SeqFrom: newSeq, SeqTo: MaxSeq,
			Op: core.OpDelete, Commit: id,
			Changed: core.MaskOf(current[pk], nil, e.Schema.Cols),
		})
	}

	b.HeadSeq = newSeq
	b.HeadCommit = id
	e.recordCommit(id, branch, newSeq, parents)
	return id
}

// Merge merges `from` into `into`; ours is the target, theirs is the source.
func (e *Engine) Merge(from, into string) (MergeResult, error) {
	fb, ok := e.branches[from]
	if !ok {
		return MergeResult{}, fmt.Errorf("engine: no branch %q", from)
	}
	tb, ok := e.branches[into]
	if !ok {
		return MergeResult{}, fmt.Errorf("engine: no branch %q", into)
	}
	bases := e.MergeBase(tb.HeadCommit, fb.HeadCommit)
	if len(bases) != 1 {
		return MergeResult{}, fmt.Errorf("engine: %d merge bases", len(bases))
	}
	base := bases[0]

	result, conflicts := e.mergeStates(
		e.StateAtCommit(base),
		e.Resolve(into),
		e.Resolve(from),
		e.maskSince(into, base),
		e.maskSince(from, base),
	)
	mr := MergeResult{Base: base, Result: result, Conflicts: conflicts, Clean: len(conflicts) == 0}
	if mr.Clean {
		mr.Commit = e.applyMergeResult(into, result, []string{tb.HeadCommit, fb.HeadCommit})
	}
	return mr, nil
}

// UpdateFromParent implements DESIGN.md §9.6: merge the parent in, then advance
// the fork point so the segment chain stays a tree even though the commit DAG
// records both parents.
func (e *Engine) UpdateFromParent(branch string) (MergeResult, error) {
	b, ok := e.branches[branch]
	if !ok {
		return MergeResult{}, fmt.Errorf("engine: no branch %q", branch)
	}
	if b.Parent == "" {
		return MergeResult{}, fmt.Errorf("engine: branch %q has no parent", branch)
	}
	p := e.branches[b.Parent]
	if p.HeadCommit == b.ForkCommit {
		// Nothing to absorb.
		return MergeResult{Base: b.ForkCommit, Result: e.Resolve(branch), Clean: true}, nil
	}

	base := b.ForkCommit
	result, conflicts := e.mergeStates(
		e.StateAtCommit(base),
		e.Resolve(branch),
		e.Resolve(b.Parent),
		e.maskSince(branch, base),
		e.maskSince(b.Parent, base),
	)
	mr := MergeResult{Base: base, Result: result, Conflicts: conflicts, Clean: len(conflicts) == 0}
	if !mr.Clean {
		return mr, nil
	}

	parents := []string{b.HeadCommit, p.HeadCommit}
	// §9.6 step 4: advance the fork point BEFORE writing the result, so the new
	// versions are computed against the post-advance inherited view and the
	// overlay keeps only genuine divergence (step 5).
	//
	// Only THIS branch's chain moves. Descendants keep the tail they captured at
	// their own fork (finding F1), so absorbing the parent here cannot change
	// what any child branch resolves to.
	b.ForkSeq, b.ForkCommit = p.HeadSeq, p.HeadCommit
	b.Chain = e.inheritedChainFrom(b.Parent)
	mr.Commit = e.applyMergeResult(branch, result, parents)
	return mr, nil
}

// --- Sessions (DESIGN.md §6.2) ---

func (e *Engine) OpenSession(branch string) (string, error) {
	b, ok := e.branches[branch]
	if !ok {
		return "", fmt.Errorf("engine: no branch %q", branch)
	}
	if branch == RootBranch {
		// Invariant 8 / §6.1: no uncommitted state on the default branch.
		return "", fmt.Errorf("engine: sessions are not permitted on %s", RootBranch)
	}
	e.nextSess++
	id := fmt.Sprintf("s%d", e.nextSess)
	e.sessions[id] = &session{
		ID: id, Branch: branch, Base: b.HeadCommit, BaseSeq: b.HeadSeq, Open: true,
	}
	return id, nil
}

// SessionWrite stages a change. Staged rows carry session_id and the zero commit
// hash, and are invisible to every read that does not name the session.
func (e *Engine) SessionWrite(sid string, cs core.ChangeSet) error {
	s, ok := e.sessions[sid]
	if !ok || !s.Open {
		return fmt.Errorf("engine: no open session %q", sid)
	}
	view := e.SessionResolve(sid)
	b := e.branches[s.Branch]

	for _, ch := range cs.Sorted() {
		before, live := view[ch.PK]

		var op core.Op
		var row core.Row
		if ch.Op == core.OpDelete {
			if !live {
				continue
			}
			op, row = core.OpDelete, nil
		} else {
			op, row = core.OpUpdate, ch.Row.Clone()
			if !live {
				op = core.OpInsert
			}
		}
		mask := core.MaskOf(before, row, e.Schema.Cols)

		// Upsert the session's staged row for this key, accumulating the mask so
		// a change-and-change-back keeps its bit set.
		found := false
		for i := range e.rows {
			v := &e.rows[i]
			if v.Session == sid && v.PK == ch.PK {
				v.Op, v.Row = op, row
				v.Changed = v.Changed.Or(mask)
				found = true
				break
			}
		}
		if !found {
			e.rows = append(e.rows, Version{
				Branch: s.Branch, PK: ch.PK, SeqFrom: b.HeadSeq + 1, SeqTo: MaxSeq,
				Op: op, Session: sid, Changed: mask, Row: row,
			})
		}
		if op == core.OpDelete {
			delete(view, ch.PK)
		} else {
			view[ch.PK] = row
		}
	}
	return nil
}

// SessionResolve is the session's private view: its base commit plus its own
// staged rows.
//
// PHASE 0 FINDING F3. It resolves against the chain recorded on the session's
// BASE COMMIT, not the branch's live head or even the branch's live chain.
//
// A session is a workspace pinned to the commit it was opened at. Pinning only
// the seq is not enough: if the branch absorbs its parent via UpdateFromParent
// while the session is open, the branch's inherited chain advances and the
// session's view would change underneath the person editing in it. Resolving
// through the base commit's own chain pins the whole view.
//
// This is the same root cause as F1 — anything that pins to a point in history
// must capture the resolution chain at that moment, not re-derive it later.
// CommitSession refuses on a moved branch anyway (§6.2 step 5), so tracking the
// head would only mean showing a view that can never be committed.
func (e *Engine) SessionResolve(sid string) core.Table {
	s, ok := e.sessions[sid]
	if !ok || !s.Open {
		return nil
	}
	cm, ok := e.commits[s.Base]
	if !ok {
		return nil
	}
	return e.resolveChain(cm.Chain, sid)
}

func (e *Engine) CommitSession(sid string) (string, error) {
	s, ok := e.sessions[sid]
	if !ok || !s.Open {
		return "", fmt.Errorf("engine: no open session %q", sid)
	}
	b := e.branches[s.Branch]
	if b.HeadCommit != s.Base {
		return "", fmt.Errorf("engine: branch moved since session opened")
	}
	newSeq := b.HeadSeq + 1
	id := e.newCommitID()
	parent := b.HeadCommit

	// Stamp the staged rows with the real commit and clear session_id. A
	// metadata operation, not a data copy (DESIGN.md §5.3).
	var staged []int
	for i := range e.rows {
		if e.rows[i].Session == sid {
			staged = append(staged, i)
		}
	}
	sort.Slice(staged, func(x, y int) bool { return e.rows[staged[x]].PK < e.rows[staged[y]].PK })
	for _, i := range staged {
		e.closeOpen(s.Branch, e.rows[i].PK, newSeq)
	}
	for _, i := range staged {
		v := &e.rows[i]
		v.Session = ""
		v.Commit = id
		v.SeqFrom = newSeq
		v.SeqTo = MaxSeq
	}

	b.HeadSeq = newSeq
	b.HeadCommit = id
	s.Open = false
	e.recordCommit(id, s.Branch, newSeq, []string{parent})
	return id, nil
}

// AbandonSession drops the staged rows. Nothing was ever visible outside the
// session, so nothing needs undoing.
func (e *Engine) AbandonSession(sid string) {
	s, ok := e.sessions[sid]
	if !ok {
		return
	}
	kept := e.rows[:0]
	for _, v := range e.rows {
		if v.Session != sid {
			kept = append(kept, v)
		}
	}
	e.rows = kept
	s.Open = false
}

// --- Diff (DESIGN.md §8.1) ---

// Diff is the interval scan: only versions whose boundaries fall in the range
// are visited, so cost is proportional to the change, not the table.
func (e *Engine) Diff(branch string, fromSeq, toSeq int64) core.ChangeSet {
	from := e.resolveChain(e.chainFor(branch, fromSeq), "")
	out := core.ChangeSet{}
	touched := map[core.PK]bool{}
	for _, v := range e.rows {
		if v.Branch != branch || v.Session != "" {
			continue
		}
		if v.SeqFrom > fromSeq && v.SeqFrom <= toSeq {
			touched[v.PK] = true
		}
	}
	to := e.resolveChain(e.chainFor(branch, toSeq), "")
	for pk := range touched {
		before, hadBefore := from[pk]
		after, hasAfter := to[pk]
		switch {
		case hadBefore && !hasAfter:
			out[pk] = core.Change{PK: pk, Op: core.OpDelete}
		case !hadBefore && hasAfter:
			out[pk] = core.Change{PK: pk, Op: core.OpInsert, Row: after.Clone(),
				Changed: core.MaskOf(nil, after, e.Schema.Cols)}
		case hadBefore && hasAfter && !before.Equal(after):
			out[pk] = core.Change{PK: pk, Op: core.OpUpdate, Row: after.Clone(),
				Changed: core.MaskOf(before, after, e.Schema.Cols)}
		}
	}
	return out
}

// --- Invariant probes for the property harness ---

// OpenVersionsPerKey reports, per branch, how many open versions each key has.
// Property invariant 4 requires exactly one.
func (e *Engine) OpenVersionsPerKey() map[string]map[core.PK]int {
	out := map[string]map[core.PK]int{}
	for _, v := range e.rows {
		if v.Session != "" || v.SeqTo != MaxSeq {
			continue
		}
		if out[v.Branch] == nil {
			out[v.Branch] = map[core.PK]int{}
		}
		out[v.Branch][v.PK]++
	}
	return out
}

// IntervalsSane checks that no key on a branch has overlapping intervals.
func (e *Engine) IntervalsSane() error {
	type key struct {
		b  string
		pk core.PK
	}
	byKey := map[key][]Version{}
	for _, v := range e.rows {
		if v.Session != "" {
			continue
		}
		k := key{v.Branch, v.PK}
		byKey[k] = append(byKey[k], v)
	}
	for k, vs := range byKey {
		sort.Slice(vs, func(i, j int) bool { return vs[i].SeqFrom < vs[j].SeqFrom })
		for i := 1; i < len(vs); i++ {
			if vs[i].SeqFrom < vs[i-1].SeqTo {
				return fmt.Errorf("overlapping intervals for %s/%s: [%d,%d) and [%d,%d)",
					k.b, k.pk, vs[i-1].SeqFrom, vs[i-1].SeqTo, vs[i].SeqFrom, vs[i].SeqTo)
			}
		}
	}
	return nil
}

// StagedOnBranch reports staged (session) rows on a branch — must always be zero
// for the root branch. Property invariant 8.
func (e *Engine) StagedOnBranch(branch string) int {
	n := 0
	for _, v := range e.rows {
		if v.Branch == branch && v.Session != "" {
			n++
		}
	}
	return n
}

// ZeroHashOnBranch reports versions on a branch with no commit id — the sidecar
// equivalent of the zero hash. Must be zero for the root branch (invariant 8).
func (e *Engine) ZeroHashOnBranch(branch string) int {
	n := 0
	for _, v := range e.rows {
		if v.Branch == branch && v.Commit == "" && v.Session == "" {
			n++
		}
	}
	return n
}

// PlanUpdateWhere builds the change set a predicate update would produce
// (DESIGN.md §7.4). It resolves matching rows through the two-pass form, applies
// the assignment to each resolved row, and emits one per-key change per match.
//
// Property invariant 12 requires this to equal the change set the equivalent
// list of per-key Update calls would produce.
func (e *Engine) PlanUpdateWhere(
	branch string,
	pred func(core.Row) bool,
	assign func(core.Row) core.Row,
) core.ChangeSet {
	matched := e.ResolveFiltered(branch, pred)
	cs := core.ChangeSet{}
	for pk, row := range matched {
		next := assign(row.Clone())
		if !next.Equal(row) {
			cs[pk] = core.Change{PK: pk, Op: core.OpUpdate, Row: next}
		}
	}
	return cs
}
