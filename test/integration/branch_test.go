package integration

import (
	"testing"

	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// TestBranchIsZeroCopyAndIsolated: creating a branch copies no data, and writing
// to it must not touch the live table (DESIGN.md G4, §6.2).
func TestBranchIsZeroCopyAndIsolated(t *testing.T) {
	f := setup(t)

	before := f.sidecarRows(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "q4-pricing", store.DefaultBranch, principal); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if after := f.sidecarRows(t); after != before {
		t.Errorf("branch creation copied data: sidecar went from %d to %d rows", before, after)
	}

	// A commit on the branch.
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "q4-pricing", Author: principal,
		Message: "raise outdoor prices",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "outdoor", "268.92", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// The live table is untouched: branch activity cannot affect production.
	if got := f.livePrice(t, "TENT-4P"); got != "249.00" {
		t.Errorf("a branch commit changed the live table: price is %s, want 249.00", got)
	}
	// The default branch still resolves to the old value.
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "249" {
		t.Errorf("default branch price is %s, want 249", got)
	}
	// The branch resolves to the new one, inheriting everything it did not change.
	if got := f.priceOn(t, "q4-pricing", "TENT-4P"); got != "268.92" {
		t.Errorf("branch price is %s, want 268.92", got)
	}
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{Branch: "q4-pricing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("branch resolved %d rows, want 3 (it must inherit what it did not change)", len(rows))
	}
}

// TestBranchDeleteDoesNotResurface is the §7.3 tombstone hazard across a real
// segment chain: a branch-level delete must mask the row inherited from main.
func TestBranchDeleteDoesNotResurface(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "cleanup", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "cleanup", Author: principal,
		Message: "discontinue the stove",
		Changes: []store.Change{{PK: f.pk(t, "STOVE-V1"), Op: core.OpDelete}},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{Branch: "cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text == "STOVE-V1" {
			t.Fatal("a branch-level delete did not mask the row inherited from main (§7.3)")
		}
	}
	if len(rows) != 2 {
		t.Errorf("branch resolved %d rows after a delete, want 2", len(rows))
	}
	// main is unaffected.
	mainRows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mainRows) != 3 {
		t.Errorf("the default branch lost a row to a branch delete: %d rows, want 3", len(mainRows))
	}
}

// TestFilteredBranchReadDoesNotResurface is the §7.3 predicate hazard across a
// real chain: a row the branch edited out of the filter's range must not
// reappear from the parent.
func TestFilteredBranchReadDoesNotResurface(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "recat", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	// STOVE-V1 is 'outdoor' on main; the branch moves it to 'kitchen'.
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "recat", Author: principal,
		Message: "recategorize",
		Changes: []store.Change{{PK: f.pk(t, "STOVE-V1"), Op: core.OpUpdate,
			Row: f.row("STOVE-V1", "Camp stove", "kitchen", "89.50", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}

	catCol := f.table.Columns[2].ID
	filtered, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{
		Branch: "recat",
		Filter: eq(catCol, "outdoor"),
	})
	if err != nil {
		t.Fatalf("filtered branch read: %v", err)
	}
	for _, r := range filtered {
		if r.Get(f.table.Columns[0].ID).Text == "STOVE-V1" {
			t.Fatal("the branch moved this row out of 'outdoor', but main's stale version resurfaced (§7.3)")
		}
	}
	if len(filtered) != 1 { // only TENT-4P remains outdoor on this branch
		t.Errorf("filtered branch read returned %d rows, want 1", len(filtered))
	}
}

// TestSessionsAreInvisibleUntilCommitted is DESIGN.md §6.2 and property
// invariant 9.
func TestSessionsAreInvisibleUntilCommitted(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "curate", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.OpenSession(f.ctx, f.repo, "curate", principal)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	if err := f.store.SessionWrite(f.ctx, f.repo, f.table, sess.ID, []store.Change{{
		PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
		Row: f.row("MUG-01", "Enamel mug", "kitchen", "15.00", "2026-08-14T00:00:00Z"),
	}}); err != nil {
		t.Fatalf("session write: %v", err)
	}

	// The session sees its own work.
	inSession, err := f.store.SessionResolve(f.ctx, f.repo, f.table, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := priceIn(f, inSession, "MUG-01"); got != "15" {
		t.Errorf("the session does not see its own staged write: price is %s, want 15", got)
	}
	// Nobody else does — not the branch, not the default branch, not the live table.
	if got := f.priceOn(t, "curate", "MUG-01"); got != "12" {
		t.Errorf("staged work leaked to the branch: price is %s, want 12", got)
	}
	if got := f.priceOn(t, store.DefaultBranch, "MUG-01"); got != "12" {
		t.Errorf("staged work leaked to the default branch: price is %s, want 12", got)
	}
	if got := f.livePrice(t, "MUG-01"); got != "12.00" {
		t.Errorf("staged work reached the live table: price is %s, want 12.00", got)
	}

	// Committing publishes it.
	if _, err := f.store.CommitSession(f.ctx, f.repo, f.table, sess.ID,
		principal, "curated mug pricing"); err != nil {
		t.Fatalf("commit session: %v", err)
	}
	if got := f.priceOn(t, "curate", "MUG-01"); got != "15" {
		t.Errorf("after committing the session, branch price is %s, want 15", got)
	}
	if got := f.livePrice(t, "MUG-01"); got != "12.00" {
		t.Errorf("committing a session on a branch touched the live table: %s", got)
	}
}

// TestSessionsRefusedOnDefaultBranch: DESIGN.md §6.1 admits no uncommitted state
// there, because the live table must be a valid commit at every instant.
func TestSessionsRefusedOnDefaultBranch(t *testing.T) {
	f := setup(t)
	_, err := f.store.OpenSession(f.ctx, f.repo, store.DefaultBranch, principal)
	if err == nil {
		t.Fatal("a session on the default branch must be refused")
	}
}

// TestAbandonedSessionLeavesNothing: staged rows were never visible, so dropping
// them needs no undo.
func TestAbandonedSessionLeavesNothing(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "scratch", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	before := f.sidecarRows(t)
	sess, err := f.store.OpenSession(f.ctx, f.repo, "scratch", principal)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SessionWrite(f.ctx, f.repo, f.table, sess.ID, []store.Change{{
		PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
		Row: f.row("MUG-01", "Enamel mug", "kitchen", "99.00", "2026-08-14T00:00:00Z"),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbandonSession(f.ctx, f.table, sess.ID); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if after := f.sidecarRows(t); after != before {
		t.Errorf("abandoning a session left %d rows behind", after-before)
	}
}

// TestChainDepthCapped: §18. Reads must not degrade unboundedly with fork depth.
func TestChainDepthCapped(t *testing.T) {
	f := setup(t)
	parent := store.DefaultBranch
	for i := 0; i < store.MaxChainDepth+2; i++ {
		name := "b" + string(rune('a'+i))
		_, err := f.store.CreateBranch(f.ctx, f.repo, name, parent, principal)
		if err != nil {
			if i < store.MaxChainDepth-1 {
				t.Fatalf("branch %d refused too early: %v", i, err)
			}
			if !contains(err.Error(), "depth") {
				t.Errorf("the refusal should name the depth cap, got: %v", err)
			}
			return
		}
		parent = name
	}
	t.Fatalf("branch creation was never refused; the depth cap of %d is not enforced", store.MaxChainDepth)
}

// TestMergeBaseFindsTheForkPoint (M2.7, §9.1).
func TestMergeBaseFindsTheForkPoint(t *testing.T) {
	f := setup(t)
	forkPoint, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "before the fork",
		Changes: []store.Change{{PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
			Row: f.row("MUG-01", "Enamel mug", "kitchen", "13.00", "2026-04-01T00:00:00Z")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "feature", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	branchHead, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "feature", Author: principal,
		Message: "on the branch",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "outdoor", "260.00", "2026-05-01T00:00:00Z")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mainHead, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "on main after the fork",
		Changes: []store.Change{{PK: f.pk(t, "STOVE-V1"), Op: core.OpUpdate,
			Row: f.row("STOVE-V1", "Camp stove", "outdoor", "95.00", "2026-05-01T00:00:00Z")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	bases, err := f.store.MergeBase(f.ctx, f.repo, mainHead.ID, branchHead.ID)
	if err != nil {
		t.Fatalf("merge base: %v", err)
	}
	if len(bases) != 1 {
		t.Fatalf("expected exactly one merge base, got %d", len(bases))
	}
	if bases[0] != forkPoint.ID {
		t.Errorf("merge base is %s, want the fork point %s", bases[0].Short(), forkPoint.ID.Short())
	}
}

// --- fixture helpers used only by branch tests ---

func (f *fixture) sidecarRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.Direct().QueryRow(f.ctx, `SELECT count(*) FROM datagit_v_products`).Scan(&n); err != nil {
		t.Fatalf("sidecar count: %v", err)
	}
	return n
}

func (f *fixture) priceOn(t *testing.T, branch, sku string) string {
	t.Helper()
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{Branch: branch})
	if err != nil {
		t.Fatalf("read %s: %v", branch, err)
	}
	return priceIn(f, rows, sku)
}

func priceIn(f *fixture, rows []core.Row, sku string) string {
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text == sku {
			return r.Get(f.table.Columns[3].ID).Text
		}
	}
	return "(absent)"
}
