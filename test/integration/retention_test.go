package integration

import (
	"fmt"
	"testing"

	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// TestPruneKeepsTheRecordThatSomethingHappened (§13.1). Retention removes
// intermediate VALUES, but history must never claim a row was unchanged over a
// period when it was — so the surviving version's interval is extended to cover
// the gap.
func TestPruneKeepsTheRecordThatSomethingHappened(t *testing.T) {
	f := setup(t)
	for i := 0; i < 6; i++ {
		f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", fmt.Sprintf("%d.00", 250+i))
	}
	before := f.sidecarRows(t)

	rep, err := f.store.Prune(f.ctx, f.repo, f.table, store.RetentionPolicy{KeepCommits: 2})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.VersionsRemoved == 0 {
		t.Fatal("prune removed nothing with six commits and a two-commit policy")
	}
	if after := f.sidecarRows(t); after >= before {
		t.Errorf("sidecar did not shrink: %d -> %d", before, after)
	}

	// The current state is untouched: an open version is never a prune candidate.
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "255" {
		t.Errorf("prune changed the current state: price is %s, want 255", got)
	}
	if got := f.livePrice(t, "TENT-4P"); got != "255.00" {
		t.Errorf("prune changed the live table: %s", got)
	}

	// The intervals are still consistent: no gaps, no overlaps.
	rep2, err := f.store.VerifyIntervals(f.ctx, f.table)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Overlaps) > 0 || len(rep2.MultipleOpen) > 0 {
		t.Errorf("prune left the sidecar inconsistent: %+v", rep2)
	}
}

// TestGCReclaimsDeletedBranchVersions (§13.2).
func TestGCReclaimsDeletedBranchVersions(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "scratch", "TENT-4P", "outdoor", "999.00")
	withBranch := f.sidecarRows(t)

	if err := f.store.DeleteBranch(f.ctx, f.repo, "scratch"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	rep, err := f.store.GC(f.ctx, f.repo)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if rep.OrphanVersions == 0 {
		t.Error("gc reclaimed nothing after a branch deletion")
	}
	if after := f.sidecarRows(t); after >= withBranch {
		t.Errorf("gc did not shrink the sidecar: %d -> %d", withBranch, after)
	}
	// main is untouched.
	if got := f.priceOn(t, store.DefaultBranch, "TENT-4P"); got != "249" {
		t.Errorf("gc changed the default branch: price is %s", got)
	}
}

// TestPurgeMarksCommitsRatherThanRehashing is the §13.4 rule that makes the
// audit trail honest: a purge deliberately breaks the hash chain and records the
// discontinuity, so "an authorized erasure happened here" stays distinguishable
// from "someone tampered with this".
func TestPurgeMarksCommitsRatherThanRehashing(t *testing.T) {
	f := setup(t)
	f.commitOn(t, store.DefaultBranch, "MUG-01", "kitchen", "14.00")

	rec, err := f.store.Purge(f.ctx, f.repo, f.table, f.pk(t, "MUG-01"),
		"GDPR erasure request 2026-09-01", "dpo@example.com")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if rec.VersionsRemoved == 0 {
		t.Error("purge removed no versions")
	}
	if rec.CommitsMarked == 0 {
		t.Error("purge marked no commits: the discontinuity must be recorded")
	}

	// The row is gone from the live table and from resolution.
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text == "MUG-01" {
			t.Error("the purged row is still resolvable")
		}
	}

	// The affected commits are marked purged, NOT re-hashed.
	var purged int
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT count(*) FROM datagit_commit WHERE integrity='purged'`).Scan(&purged))
	if purged == 0 {
		t.Error("no commit was marked purged")
	}

	// A tombstone records that it happened, with a reason and an actor, and
	// never the purged content.
	var reason, by string
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT reason, purged_by FROM datagit_purge_log LIMIT 1`).Scan(&reason, &by))
	if reason == "" || by != "dpo@example.com" {
		t.Errorf("tombstone is incomplete: reason=%q by=%q", reason, by)
	}

	// Verification still passes: purged commits are skipped, not silently
	// accepted as intact.
	if err := f.store.VerifyIntegrity(f.ctx, f.repo, store.DefaultBranch); err != nil {
		t.Errorf("verify should skip purged commits, not fail on them: %v", err)
	}
}

// TestPurgeRequiresAReason: it is audited and irreversible.
func TestPurgeRequiresAReason(t *testing.T) {
	f := setup(t)
	if _, err := f.store.Purge(f.ctx, f.repo, f.table, f.pk(t, "MUG-01"), "  ", "dpo@example.com"); err == nil {
		t.Fatal("purge without a stated reason must be refused")
	}
}

// TestVerifyIntervalsPassesOnHealthyData, and catches a hand-corrupted sidecar.
func TestVerifyIntervalsDetectsCorruption(t *testing.T) {
	f := setup(t)
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "260.00")

	rep, err := f.store.VerifyIntervals(f.ctx, f.table)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Overlaps) > 0 || len(rep.MultipleOpen) > 0 {
		t.Fatalf("healthy sidecar reported problems: %+v", rep)
	}

	// Corrupt it: reopen a closed interval, so the key now has two open versions.
	must(t, f.pool.Direct().Exec(f.ctx, fmt.Sprintf(
		`UPDATE datagit_v_products SET seq_to = %d WHERE seq_to <> %d`,
		store.MaxSeqValue, store.MaxSeqValue)))

	rep, err = f.store.VerifyIntervals(f.ctx, f.table)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.MultipleOpen) == 0 {
		t.Error("two open versions for one key went undetected; every read depends on there being exactly one")
	}
}

var _ = core.OpUpdate

// TestGuardedModeRejectsOutOfBandWrites (§6.3). The invariant that the live
// table is main@HEAD holds only while writes go through DataGit; guarded mode
// enforces that for tables that can afford to break legacy writers.
func TestGuardedModeRejectsOutOfBandWrites(t *testing.T) {
	f := setup(t)
	if err := f.store.SetWriteMode(f.ctx, f.table, store.ModeGuarded); err != nil {
		t.Fatalf("set guarded: %v", err)
	}

	// A direct write, exactly what a psql session or a legacy job does.
	err := f.pool.Direct().Exec(f.ctx, `UPDATE products SET price = 999.99 WHERE sku = 'MUG-01'`)
	if err == nil {
		t.Fatal("guarded mode did not reject an out-of-band write")
	}
	if !contains(err.Error(), "guarded mode") {
		t.Errorf("the rejection should explain itself, got: %v", err)
	}

	// DataGit's own writes still pass, because its transactions set the marker.
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "a legitimate change",
		Changes: []store.Change{{PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
			Row: f.row("MUG-01", "Enamel mug", "kitchen", "13.00", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatalf("guarded mode blocked DataGit's own write: %v", err)
	}
	if got := f.livePrice(t, "MUG-01"); got != "13.00" {
		t.Errorf("live price is %s, want 13.00", got)
	}
}

// TestCaptureModeRecordsOutOfBandWrites (§6.3). A trigger has no author, no
// message, and no commit boundary, so capture records THAT a write happened and
// leaves reconciliation to the drift verifier.
func TestCaptureModeRecordsOutOfBandWrites(t *testing.T) {
	f := setup(t)
	if err := f.store.SetWriteMode(f.ctx, f.table, store.ModeCapture); err != nil {
		t.Fatalf("set capture: %v", err)
	}
	must(t, f.pool.Direct().Exec(f.ctx, `UPDATE products SET price = 999.99 WHERE sku = 'MUG-01'`))

	n, err := f.store.DriftEvents(f.ctx, f.table)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("capture mode recorded nothing for an out-of-band write")
	}
	// And the drift scan agrees.
	rep, err := f.store.VerifyDrift(f.ctx, f.repo, f.table)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mismatched == 0 {
		t.Error("the drift scan did not see the out-of-band write")
	}
}

// TestOpenModeAddsNoTriggers is the default and the happy path: DESIGN.md §5.1
// requires the live table to carry no triggers on the happy path.
func TestOpenModeAddsNoTriggers(t *testing.T) {
	f := setup(t)
	if err := f.store.SetWriteMode(f.ctx, f.table, store.ModeOpen); err != nil {
		t.Fatal(err)
	}
	if n := f.liveTriggerCount(t); n != 0 {
		t.Errorf("open mode left %d trigger(s) on the live table; the happy path must add none", n)
	}
}
