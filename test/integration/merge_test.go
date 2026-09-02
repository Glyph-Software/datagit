package integration

import (
	"testing"

	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// branchWith forks a branch and commits one row change on it.
func (f *fixture) branchWith(t *testing.T, name, sku, category, price string) {
	t.Helper()
	if _, err := f.store.CreateBranch(f.ctx, f.repo, name, store.DefaultBranch, principal); err != nil {
		t.Fatalf("create branch %s: %v", name, err)
	}
	f.commitOn(t, name, sku, category, price)
}

func (f *fixture) commitOn(t *testing.T, branch, sku, category, price string) {
	t.Helper()
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text == sku {
			name = r.Get(f.table.Columns[1].ID).Text
		}
	}
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: branch, Author: principal,
		Message: "change " + sku + " on " + branch,
		Changes: []store.Change{{PK: f.pk(t, sku), Op: core.OpUpdate,
			Row: f.row(sku, name, category, price, "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatalf("commit on %s: %v", branch, err)
	}
}

// TestMergeDisjointRows: two branches touching different rows merge without
// human involvement.
func TestMergeDisjointRows(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "pricing", "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "pricing", store.DefaultBranch,
		principal, "merge pricing", true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !res.Clean {
		t.Fatalf("a single-branch change should merge clean, got %d conflicts: %v",
			len(res.Conflicts), res.Conflicts)
	}
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "268.92" {
		t.Errorf("after merge the default branch price is %s, want 268.92", got)
	}
	// The merge applied to the live table, because main's live table IS main@HEAD.
	if got := f.livePrice(t, "TENT-4P"); got != "268.92" {
		t.Errorf("the merge did not reach the live table: %s", got)
	}
}

// TestMergeDisjointCellsMergesPerCell is the reason cell-level merge exists
// (§9.2): two curators editing different columns of the same row must not
// collide.
func TestMergeDisjointCellsMergesPerCell(t *testing.T) {
	f := setup(t)
	catCol, priceCol := f.table.Columns[2].ID, f.table.Columns[3].ID

	if _, err := f.store.CreateBranch(f.ctx, f.repo, "recat", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	// The branch changes only the category.
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "recat", Author: principal,
		Message: "recategorize",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "camping", "249.00", "2026-03-02T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}
	// main changes only the price, on the same row.
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "recat", store.DefaultBranch,
		principal, "merge recat", true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !res.Clean {
		t.Fatalf("disjoint cell edits must merge clean, got conflicts: %v", res.Conflicts)
	}

	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text != "TENT-4P" {
			continue
		}
		if got := r.Get(catCol).Text; got != "camping" {
			t.Errorf("merged category is %q, want camping (from the branch)", got)
		}
		if got := r.Get(priceCol).Text; got != "268.92" {
			t.Errorf("merged price is %s, want 268.92 (from main)", got)
		}
	}
}

// TestMergeSameCellDifferentValuesConflicts (§9.2).
func TestMergeSameCellDifferentValuesConflicts(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "cheap", "TENT-4P", "outdoor", "199.00")
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "cheap", store.DefaultBranch,
		principal, "", false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Clean {
		t.Fatal("two branches setting the same cell to different values must conflict")
	}
	found := false
	for _, c := range res.Conflicts {
		if c.Kind == core.ConflictCell {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a cell conflict, got %v", res.Conflicts)
	}
	// Nothing was applied.
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "268.92" {
		t.Errorf("a conflicted merge changed the target: price is %s", got)
	}
}

// TestMergeSameCellSameValueIsClean (§9.2): agreeing is not conflicting.
func TestMergeSameCellSameValueIsClean(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "agree", "TENT-4P", "outdoor", "268.92")
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "agree", store.DefaultBranch,
		principal, "merge agree", true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !res.Clean {
		t.Errorf("both sides making the identical change must merge clean, got %v", res.Conflicts)
	}
}

// TestMergeDeleteModifyAlwaysConflicts (§9.2). Neither answer is safe to assume:
// one side believes the row should not exist, the other that it should exist
// with new content.
func TestMergeDeleteModifyAlwaysConflicts(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "discontinue", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "discontinue", Author: principal,
		Message: "discontinue the tent",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpDelete}},
	}); err != nil {
		t.Fatal(err)
	}
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "discontinue", store.DefaultBranch,
		principal, "", false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Clean {
		t.Fatal("delete on one side and modify on the other must always conflict")
	}
	if res.Conflicts[0].Kind != core.ConflictDeleteModify {
		t.Errorf("conflict kind is %s, want delete_modify", res.Conflicts[0].Kind)
	}
}

// TestMergeChangeAndChangeBackIsClean is Phase 0 finding F2 at the database
// level: changed_cols is a SUPERSET, so an overlapping mask must not by itself
// produce a conflict.
func TestMergeChangeAndChangeBackIsClean(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "wobble", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	// The branch changes the price and changes it straight back. Its mask now
	// marks the price column, but the value equals the base.
	f.commitOn(t, "wobble", "TENT-4P", "outdoor", "300.00")
	f.commitOn(t, "wobble", "TENT-4P", "outdoor", "249.00")
	// main changes the same column, once.
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "wobble", store.DefaultBranch,
		principal, "merge wobble", true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !res.Clean {
		t.Fatalf("change-and-change-back must not conflict: the mask is a superset, "+
			"so every decision is made by comparing values (finding F2). Got: %v", res.Conflicts)
	}
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "268.92" {
		t.Errorf("merged price is %s, want 268.92 (main's change survives)", got)
	}
}

// TestMergeCommitRecordsBothParents keeps the DAG honest even though resolution
// walks only the chain.
func TestMergeCommitRecordsBothParents(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "feature", "MUG-01", "kitchen", "15.00")
	res, err := f.store.Merge(f.ctx, f.repo, f.table, "feature", store.DefaultBranch,
		principal, "merge feature", true)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pool.Direct().QueryRow(f.ctx,
		`SELECT array_length(parent_ids, 1) FROM datagit_commit WHERE id=$1`,
		res.Commit[:]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("merge commit has %d parents, want 2", n)
	}
}

// TestUpdateFromParentAdvancesTheForkPoint is §9.6, and the shape that produced
// Phase 0 findings F4 and F5.
func TestUpdateFromParentAdvancesTheForkPoint(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "long-lived", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	f.commitOn(t, "long-lived", "TENT-4P", "outdoor", "260.00")
	f.commitOn(t, store.DefaultBranch, "MUG-01", "kitchen", "14.00")

	res, err := f.store.UpdateFromParent(f.ctx, f.repo, f.table, "long-lived", principal)
	if err != nil {
		t.Fatalf("update from parent: %v", err)
	}
	if !res.Clean {
		t.Fatalf("absorbing a parent that touched other rows must be clean, got %v", res.Conflicts)
	}
	if got := f.priceOn(t, "long-lived", "MUG-01"); got != "14" {
		t.Errorf("branch did not absorb the parent's change: MUG-01 is %s, want 14", got)
	}
	if got := f.priceOn(t, "long-lived", "TENT-4P"); got != "260" {
		t.Errorf("branch lost its own change: TENT-4P is %s, want 260", got)
	}
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "249" {
		t.Errorf("update-from-parent modified the parent: TENT-4P is %s, want 249", got)
	}
}

// TestUpdateFromParentDoesNotMoveDescendants is Phase 0 finding F1: a branch
// absorbing its parent must not change what a branch forked from IT resolves to.
func TestUpdateFromParentDoesNotMoveDescendants(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "mid", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "leaf", "mid", principal); err != nil {
		t.Fatal(err)
	}
	before := f.priceOn(t, "leaf", "MUG-01")

	// main moves, then mid absorbs it. `leaf` asked for nothing.
	f.commitOn(t, store.DefaultBranch, "MUG-01", "kitchen", "99.00")
	if _, err := f.store.UpdateFromParent(f.ctx, f.repo, f.table, "mid", principal); err != nil {
		t.Fatalf("update mid: %v", err)
	}

	if after := f.priceOn(t, "leaf", "MUG-01"); after != before {
		t.Errorf("a descendant's state changed because its parent absorbed ITS parent: "+
			"MUG-01 went from %s to %s. The chain must be captured at fork (finding F1)",
			before, after)
	}
}

// TestConflictsArePersisted (§9.4): a half-resolved merge must survive a
// restart, so conflicts are rows rather than in-memory state.
func TestConflictsArePersisted(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "cheap", "TENT-4P", "outdoor", "199.00")
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	res, err := f.store.Merge(f.ctx, f.repo, f.table, "cheap", store.DefaultBranch, principal, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("expected conflicts")
	}

	var pid int64
	if err := f.pool.Direct().QueryRow(f.ctx,
		`INSERT INTO datagit_proposal (repo_id, from_ref, into_ref, title, state, created_by)
		 SELECT $1, fr.id, ir.id, 'test', 'conflicted', $2
		   FROM datagit_ref fr, datagit_ref ir
		  WHERE fr.repo_id=$1 AND fr.name='cheap' AND ir.repo_id=$1 AND ir.name='main'
		 RETURNING id`, f.repo.ID, principal).Scan(&pid); err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	if err := f.store.SaveConflicts(f.ctx, pid, f.table, res.Conflicts); err != nil {
		t.Fatalf("save conflicts: %v", err)
	}
	var n int
	if err := f.pool.Direct().QueryRow(f.ctx,
		`SELECT count(*) FROM datagit_conflict WHERE proposal_id=$1`, pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(res.Conflicts) {
		t.Errorf("persisted %d conflicts, computed %d", n, len(res.Conflicts))
	}
}

// TestMergeCommitVerifies: a merge commit's id must be computed over BOTH
// parents, or recomputing the chain fails on every merge — which would make
// `verify --integrity` useless precisely where history is most complex.
func TestMergeCommitVerifies(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "feature", "MUG-01", "kitchen", "15.00")
	if _, err := f.store.Merge(f.ctx, f.repo, f.table, "feature", store.DefaultBranch,
		principal, "merge feature", true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.VerifyIntegrity(f.ctx, f.repo, store.DefaultBranch); err != nil {
		t.Errorf("hash chain does not verify after a merge: %v", err)
	}
}
