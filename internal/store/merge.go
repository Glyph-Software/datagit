package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// MergeResult is the outcome of a three-way merge.
//
// When Clean is false nothing was applied: DESIGN.md §9.4 surfaces conflicts
// rather than guessing, and §9.3 applies only a fully clean, fully validated
// merge.
type MergeResult struct {
	Base      hash.Digest
	Clean     bool
	Conflicts []core.Conflict
	Changes   []Change
	Commit    hash.Digest
	Applied   int
}

// Merge performs a three-way merge of `from` into `into` (M3, §9.2).
//
// Ours is the target, theirs is the source — the same orientation Git uses.
func (s *Store) Merge(ctx context.Context, repo *Repo, t *Table, from, into, author, message string,
	apply bool) (*MergeResult, error) {

	tx := s.pool.Direct()
	fromID, fromHead, _, fromChain, err := s.loadRef(ctx, tx, repo, from)
	if err != nil {
		return nil, err
	}
	_, intoHead, _, intoChain, err := s.loadRef(ctx, tx, repo, into)
	if err != nil {
		return nil, err
	}
	_ = fromID

	bases, err := s.MergeBase(ctx, repo, intoHead, fromHead)
	if err != nil {
		return nil, err
	}
	switch len(bases) {
	case 1:
	case 0:
		return nil, fmt.Errorf("no common ancestor between %q and %q", into, from)
	default:
		// DESIGN.md §9.1: refuse and name the candidates. Silently picking one
		// produces a result that is wrong in a way nobody notices until it
		// matters, so recursive base merging is deferred rather than approximated.
		names := make([]string, 0, len(bases))
		for _, b := range bases {
			names = append(names, b.Short())
		}
		sort.Strings(names)
		return nil, fmt.Errorf(
			"merge of %q into %q has %d merge bases (%s): merge in a different order. "+
				"Recursive base merging is deferred rather than approximated (§9.1)",
			from, into, len(bases), strings.Join(names, ", "))
	}
	base := bases[0]

	baseChain, err := s.chainAt(ctx, tx, base)
	if err != nil {
		return nil, err
	}
	baseState, err := s.resolveMap(ctx, tx, t, baseChain, nil)
	if err != nil {
		return nil, err
	}
	ours, err := s.resolveMap(ctx, tx, t, intoChain, nil)
	if err != nil {
		return nil, err
	}
	theirs, err := s.resolveMap(ctx, tx, t, fromChain, nil)
	if err != nil {
		return nil, err
	}

	maskO, err := s.maskSince(ctx, tx, t, intoChain, baseChain)
	if err != nil {
		return nil, err
	}
	maskT, err := s.maskSince(ctx, tx, t, fromChain, baseChain)
	if err != nil {
		return nil, err
	}

	merged, conflicts := mergeStates(t, baseState, ours, theirs, maskO, maskT)
	res := &MergeResult{Base: base, Clean: len(conflicts) == 0, Conflicts: conflicts}

	// The change set is the difference between the merged result and the target.
	for pk, row := range merged {
		cur, live := ours[pk]
		if live && cur.Equal(row) {
			continue
		}
		op := core.OpUpdate
		if !live {
			op = core.OpInsert
		}
		res.Changes = append(res.Changes, Change{PK: pk, Op: op, Row: row})
	}
	for pk := range ours {
		if _, still := merged[pk]; !still {
			if _, conflicted := conflictedKeys(conflicts)[pk]; conflicted {
				continue
			}
			res.Changes = append(res.Changes, Change{PK: pk, Op: core.OpDelete})
		}
	}
	sort.Slice(res.Changes, func(i, j int) bool { return res.Changes[i].PK < res.Changes[j].PK })

	if !res.Clean || !apply {
		return res, nil
	}
	if message == "" {
		message = fmt.Sprintf("Merge %s into %s", from, into)
	}
	// The merge commit records BOTH parents, so the DAG stays honest even though
	// resolution walks only the chain (§7.3). The second parent is part of the
	// commit HASH, so the chain still verifies across merges.
	cr, err := s.Commit(ctx, CommitRequest{
		Repo: repo, Table: t, Branch: into, Changes: res.Changes,
		Author: author, Message: message, ExtraParents: []hash.Digest{fromHead},
	})
	if err != nil {
		return nil, err
	}
	res.Commit, res.Applied = cr.ID, cr.Changed
	return res, nil
}

func conflictedKeys(cs []core.Conflict) map[core.PK]bool {
	out := map[core.PK]bool{}
	for _, c := range cs {
		out[c.PK] = true
	}
	return out
}

// mergeStates is DESIGN.md §9.2's case table, with mask-narrowed cell scanning.
//
// The masks narrow WHICH COLUMNS are examined and nothing more. Every decision
// is made by comparing values, because a set bit does not imply a changed value:
// a branch that changes a column and changes it back leaves the bit set
// (finding F2). Disjoint masks are sufficient for a clean merge; overlapping
// masks are NOT sufficient for a conflict.
//
// Columns outside the union of both masks were touched by neither side and
// therefore still hold the base value.
func mergeStates(t *Table, base, ours, theirs map[core.PK]core.Row,
	maskO, maskT map[core.PK]core.ColMask) (map[core.PK]core.Row, []core.Conflict) {

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

	out := map[core.PK]core.Row{}
	var conflicts []core.Conflict

	for _, pk := range sorted {
		b, bOK := base[pk]
		a, aOK := ours[pk]
		th, tOK := theirs[pk]

		aChanged := !sameOpt(b, bOK, a, aOK)
		tChanged := !sameOpt(b, bOK, th, tOK)

		switch {
		case !aChanged && !tChanged:
			if bOK {
				out[pk] = b.Clone()
			}
		case !aChanged: // only theirs moved
			if tOK {
				out[pk] = th.Clone()
			}
		case !tChanged: // only ours moved
			if aOK {
				out[pk] = a.Clone()
			}
		case sameOpt(a, aOK, th, tOK): // identical change, including delete/delete
			if aOK {
				out[pk] = a.Clone()
			}
		case !bOK:
			conflicts = append(conflicts, core.Conflict{PK: pk, Kind: core.ConflictAddAdd})
		case !aOK || !tOK:
			// Delete/modify is ALWAYS a conflict. One side believes the row should
			// not exist, the other that it should exist with new content; any
			// automatic resolution silently discards someone's intent.
			conflicts = append(conflicts, core.Conflict{PK: pk, Kind: core.ConflictDeleteModify})
		default:
			candidates := core.UnionCols(maskO[pk], maskT[pk])
			touched := make(map[core.ColID]bool, len(candidates))
			for _, c := range candidates {
				touched[c] = true
			}
			merged := core.Row{}
			for _, c := range t.ColIDs() {
				if !touched[c] {
					merged[c] = b.Get(c)
				}
			}
			var cc []core.Conflict
			for _, c := range candidates {
				bv, av, tv := b.Get(c), a.Get(c), th.Get(c)
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

func sameOpt(a core.Row, aOK bool, b core.Row, bOK bool) bool {
	if aOK != bOK {
		return false
	}
	if !aOK {
		return true
	}
	return a.Equal(b)
}

// maskSince accumulates every column that may have changed along a chain since
// the base, by DIFFING the two chains segment by segment.
//
// Two Phase 0 findings are encoded here:
//
//   - F4. UpdateFromParent advances a fork point and prunes overlay rows that
//     now match the parent, so a branch's effective state can change with
//     nothing written to its own overlay. Walking branch parentage and stopping
//     at the base's branch would miss every such change.
//
//   - F5. The range is taken in BOTH directions. A branch differs from the base
//     by LACKING changes the base carries as well as by adding new ones, which
//     happens whenever a branch forked before its parent advanced. Accumulating
//     only the forward range silently keeps base values for every column in the
//     backward range.
func (s *Store) maskSince(ctx context.Context, tx adapter.Tx, t *Table,
	chain, baseChain []adapter.Segment) (map[core.PK]core.ColMask, error) {

	baseUpper := make(map[uuid.UUID]int64, len(baseChain))
	for _, seg := range baseChain {
		baseUpper[uuid.UUID(seg.BranchID)] = seg.Seq
	}
	out := map[core.PK]core.ColMask{}

	accumulate := func(branch uuid.UUID, lo, hi int64) error {
		if hi <= lo {
			return nil
		}
		sel := make([]string, 0, len(t.PKColumns))
		for _, id := range t.PKColumns {
			sel = append(sel, quote(catalog.SidecarColumn(uint32(id))))
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(
			`SELECT %s, changed_cols FROM %s
			  WHERE branch_id=$1 AND session_id IS NULL AND seq_from > $2 AND seq_from <= $3`,
			strings.Join(sel, ", "), quote(catalog.SidecarTable(t.Physical))),
			branch, lo, hi)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, len(t.PKColumns))
			dest := make([]any, 0, len(t.PKColumns)+1)
			for i := range vals {
				dest = append(dest, &vals[i])
			}
			var mask []byte
			dest = append(dest, &mask)
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			row := core.Row{}
			for i, id := range t.PKColumns {
				c, _ := t.Column(id)
				v, err := fromDriver(vals[i], c.Kind)
				if err != nil {
					return err
				}
				row[id] = v
			}
			pk := core.MakePK(row, t.PKColumns)
			out[pk] = out[pk].Or(bytesToMask(mask))
		}
		return rows.Err()
	}

	seen := map[uuid.UUID]bool{}
	for _, seg := range chain {
		b := uuid.UUID(seg.BranchID)
		seen[b] = true
		baseSeq, inBase := baseUpper[b]
		if !inBase {
			if err := accumulate(b, 0, seg.Seq); err != nil {
				return nil, err
			}
			continue
		}
		lo, hi := baseSeq, seg.Seq
		if hi < lo {
			lo, hi = hi, lo // F5: the range runs both ways
		}
		if err := accumulate(b, lo, hi); err != nil {
			return nil, err
		}
	}
	// Segments the base could see that this chain cannot see at all.
	for _, seg := range baseChain {
		b := uuid.UUID(seg.BranchID)
		if !seen[b] {
			if err := accumulate(b, 0, seg.Seq); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// resolveMap resolves a chain into a keyed map.
func (s *Store) resolveMap(ctx context.Context, tx adapter.Tx, t *Table,
	chain []adapter.Segment, session *uuid.UUID) (map[core.PK]core.Row, error) {
	spec := &adapter.ResolveSpec{Table: t.Spec(), Chain: chain}
	if session != nil {
		spec.Session = (*[16]byte)(session)
	}
	q, err := s.ad.ResolveQuery(spec)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	defer rows.Close()
	out := map[core.PK]core.Row{}
	for rows.Next() {
		r, _, err := scanRow(rows, t)
		if err != nil {
			return nil, err
		}
		out[core.MakePK(r, t.PKColumns)] = r
	}
	return out, rows.Err()
}

// UpdateFromParent absorbs the parent's newer commits into a branch and advances
// its fork point (M2.9, §9.6).
//
// The fork point advances so the segment chain stays a TREE even though the
// commit DAG records both parents. Only this branch's chain moves: descendants
// keep the tail they captured at their own fork (finding F1), so absorbing here
// cannot change what any child branch resolves to.
//
// Rebase is deliberately not offered: it rewrites commit hashes, which the audit
// use case forbids.
func (s *Store) UpdateFromParent(ctx context.Context, repo *Repo, t *Table,
	branch, author string) (*MergeResult, error) {

	tx := s.pool.Direct()
	var parentName string
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(p.name,'') FROM datagit_ref r
		   LEFT JOIN datagit_ref p ON p.id = r.parent_ref
		  WHERE r.repo_id=$1 AND r.kind='branch' AND r.name=$2`,
		repo.ID, branch).Scan(&parentName); err != nil {
		return nil, err
	}
	if parentName == "" {
		return nil, fmt.Errorf("branch %q has no parent to update from", branch)
	}

	res, err := s.Merge(ctx, repo, t, parentName, branch, author,
		fmt.Sprintf("Update %s from %s", branch, parentName), true)
	if err != nil {
		return nil, err
	}
	if !res.Clean {
		return res, nil
	}

	// §9.6 step 4: advance the fork point and this branch's chain to the parent's
	// current head.
	err = s.pool.InTx(ctx, func(tx adapter.Tx) error {
		parentID, parentHead, parentSeq, parentChain, err := s.loadRef(ctx, tx, repo, parentName)
		if err != nil {
			return err
		}
		branchID, _, headSeq, _, err := s.loadRef(ctx, tx, repo, branch)
		if err != nil {
			return err
		}
		newChain := make([]adapter.Segment, 0, len(parentChain)+1)
		newChain = append(newChain, adapter.Segment{BranchID: branchID, Seq: headSeq})
		newChain = append(newChain, adapter.Segment{BranchID: parentID, Seq: parentSeq})
		newChain = append(newChain, parentChain[1:]...)
		return tx.Exec(ctx,
			`UPDATE datagit_ref SET fork_commit=$1, fork_seq=$2, chain=$3 WHERE id=$4`,
			parentHead[:], parentSeq, mustJSON(newChain), branchID)
	})
	return res, err
}

// SaveConflicts persists a merge's conflicts so a half-resolved merge survives a
// restart, a redeploy, and a reviewer going home for the weekend (§9.4).
func (s *Store) SaveConflicts(ctx context.Context, proposalID int64, t *Table, cs []core.Conflict) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		for _, c := range cs {
			var col *int32
			if c.HasCol {
				v := int32(c.Col)
				col = &v
			}
			if err := tx.Exec(ctx,
				`INSERT INTO datagit_conflict
				   (proposal_id, table_id, pk_bytes, column_id, kind, base_value, our_value, their_value)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				proposalID, t.ID, []byte(c.PK), col, c.Kind.String(),
				c.Base.String(), c.Ours.String(), c.Theirs.String()); err != nil {
				return err
			}
		}
		return nil
	})
}
