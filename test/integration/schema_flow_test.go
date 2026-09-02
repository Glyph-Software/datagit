package integration

import (
	"strings"
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// addCol returns the fixture table's columns plus one more.
func (f *fixture) plusColumn(name, sqlType string) []adapter.Column {
	cols := append([]adapter.Column(nil), f.table.Columns...)
	kind, _ := f.ad.KindFor(sqlType)
	var next adapter.Column
	next.ID = f.table.Columns[len(f.table.Columns)-1].ID + 1
	next.Name, next.SQLType, next.Kind, next.Nullable = name, sqlType, kind, true
	return append(cols, next)
}

// TestSchemaChangeOnMainIsRefused is §10.4 at its bluntest: main's shape is what
// direct readers compiled against, so it does not change by fiat.
func TestSchemaChangeOnMainIsRefused(t *testing.T) {
	f := setup(t)
	_, err := f.store.AlterBranchSchema(f.ctx, f.repo, f.table, store.DefaultBranch,
		f.plusColumn("margin_pct", "numeric(5,2)"), principal)
	if err == nil {
		t.Fatal("altering main's schema directly was allowed; it must go through a plan")
	}
	if !strings.Contains(err.Error(), "migration plan") {
		t.Errorf("the refusal does not point at the migration plan route: %v", err)
	}
}

// TestBranchSchemaChangeLeavesTheLiveTableAlone is the invariant that makes the
// whole flow safe: a branch can change shape while main's readers see nothing.
func TestBranchSchemaChangeLeavesTheLiveTableAlone(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "wide", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	before := f.liveColumnCount(t)

	res, err := f.store.AlterBranchSchema(f.ctx, f.repo, f.table, "wide",
		f.plusColumn("margin_pct", "numeric(5,2)"), principal)
	if err != nil {
		t.Fatalf("alter branch schema: %v", err)
	}
	if res.Epoch != 1 {
		t.Errorf("schema epoch is %d, want 1", res.Epoch)
	}
	if len(res.Changes) != 1 {
		t.Errorf("recorded %d changes, want 1", len(res.Changes))
	}
	if after := f.liveColumnCount(t); after != before {
		t.Errorf("a BRANCH schema change altered the live table: %d columns, was %d", after, before)
	}

	// The branch has the new shape; main does not.
	wide, err := f.store.LoadSchema(f.ctx, f.repo, f.table, "wide")
	if err != nil {
		t.Fatal(err)
	}
	main, err := f.store.LoadSchema(f.ctx, f.repo, f.table, store.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if len(wide.Columns) != len(main.Columns)+1 {
		t.Errorf("branch has %d columns and main %d; want exactly one more on the branch",
			len(wide.Columns), len(main.Columns))
	}
}

// TestSchemaMergeProducesAPlanRatherThanApplying is CLAUDE.md invariant 9 and
// DESIGN.md §10.4: the data half merges, the shape half waits.
func TestSchemaMergeProducesAPlanRatherThanApplying(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "q4", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AlterBranchSchema(f.ctx, f.repo, f.table, "q4",
		f.plusColumn("margin_pct", "numeric(5,2)"), principal); err != nil {
		t.Fatal(err)
	}
	f.commitOn(t, "q4", "TENT-4P", "outdoor", "268.92")

	before := f.liveColumnCount(t)
	prop, err := f.store.CreateProposal(f.ctx, f.repo, "q4", store.DefaultBranch,
		"Q4 margins", "", principal)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.store.MergeProposal(f.ctx, f.repo, f.table, prop.ID, "maya@example.com")
	if err != nil {
		t.Fatalf("merge proposal: %v", err)
	}

	// The DATA merged.
	if !res.Clean {
		t.Fatalf("merge was not clean: %d conflicts", len(res.Conflicts))
	}
	if got := f.livePrice(t, "TENT-4P"); got != "268.92" {
		t.Errorf("the data half did not apply: price is %s, want 268.92", got)
	}

	// The SHAPE did not.
	if res.PendingMigration == nil {
		t.Fatal("a schema change merged with no migration plan; §10.4 requires a plan")
	}
	if after := f.liveColumnCount(t); after != before {
		t.Errorf("the schema merge changed the live table immediately: %d columns, was %d. "+
			"Direct readers get no rollout window that way (§10.4)", after, before)
	}
	if len(res.PendingMigration.Ops) == 0 {
		t.Error("the plan has no operations")
	}
	if res.PendingMigration.State != "pending" {
		t.Errorf("plan state is %q, want pending", res.PendingMigration.State)
	}

	// Applying it deliberately is what changes the live table.
	applied, err := f.store.ApplyMigrationPlan(f.ctx, f.repo, res.PendingMigration.ID,
		false, "maya@example.com")
	if err != nil {
		t.Fatalf("apply migration plan: %v", err)
	}
	if applied.State != "applied" {
		t.Errorf("plan state after apply is %q, want applied", applied.State)
	}
	if after := f.liveColumnCount(t); after != before+1 {
		t.Errorf("after apply the live table has %d columns, want %d", after, before+1)
	}
	if !f.columnExists(t, "margin_pct") {
		t.Error("the applied plan did not add margin_pct to the live table")
	}
}

// TestDestructivePlanRefusesWithoutConfirmation: a plan that breaks direct
// readers names what it would do and waits to be told.
func TestDestructivePlanRefusesWithoutConfirmation(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "slim", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	// Drop a column on the branch by leaving it out of the requested shape.
	var kept []adapter.Column
	for _, c := range f.table.Columns {
		if c.Name != "category" {
			kept = append(kept, c)
		}
	}
	if _, err := f.store.AlterBranchSchema(f.ctx, f.repo, f.table, "slim", kept, principal); err != nil {
		t.Fatal(err)
	}
	prop, err := f.store.CreateProposal(f.ctx, f.repo, "slim", store.DefaultBranch,
		"drop category", "", principal)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.store.MergeProposal(f.ctx, f.repo, f.table, prop.ID, "maya@example.com")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.PendingMigration == nil {
		t.Fatal("dropping a column produced no plan")
	}
	if len(res.PendingMigration.Confirm) == 0 {
		t.Fatal("a column drop did not ask for confirmation; §10.4 requires it")
	}

	_, err = f.store.ApplyMigrationPlan(f.ctx, f.repo, res.PendingMigration.ID,
		false, "maya@example.com")
	if err == nil {
		t.Fatal("a destructive plan applied without confirmation")
	}
	if !strings.Contains(err.Error(), "confirmation") {
		t.Errorf("the refusal does not say confirmation is needed: %v", err)
	}

	// With confirmation it runs.
	if _, err := f.store.ApplyMigrationPlan(f.ctx, f.repo, res.PendingMigration.ID,
		true, "maya@example.com"); err != nil {
		t.Fatalf("apply with confirmation: %v", err)
	}
}

// TestNarrowingForksToANewColumnID is §10.5 rule 3. History is never coerced
// through a lossy cast.
func TestNarrowingForksToANewColumnID(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "narrow", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	// price is numeric(12,2); narrowing it to numeric(8,2) cannot hold every
	// stored value.
	want := append([]adapter.Column(nil), f.table.Columns...)
	var oldID int
	for i := range want {
		if want[i].Name == "price" {
			oldID = int(want[i].ID)
			want[i].SQLType = "numeric(8,2)"
		}
	}
	res, err := f.store.AlterBranchSchema(f.ctx, f.repo, f.table, "narrow", want, principal)
	if err != nil {
		t.Fatalf("alter: %v", err)
	}
	if len(res.Forked) != 1 || res.Forked[0] != "price" {
		t.Fatalf("narrowing price did not fork to a new column id: forked %v. "+
			"Altering in place would coerce stored history through a lossy cast "+
			"(§10.5 rule 3)", res.Forked)
	}

	v, err := f.store.LoadSchema(f.ctx, f.repo, f.table, "narrow")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Columns {
		if c.Name == "price" && int(c.ID) == oldID {
			t.Error("the narrowed column kept its old id; old versions would be read through it")
		}
	}
	// The old id is recorded as dropped, not deleted: its sidecar column still
	// holds the history written under it (§10.5 rule 2).
	if _, ok := v.Dropped[core.ColID(oldID)]; !ok {
		t.Errorf("the old column id %d is not marked dropped; its history would be orphaned", oldID)
	}
}

// TestWideningAltersInPlace is the other half of rule 3: a change every stored
// value survives does NOT fork, because forking would split history for nothing.
func TestWideningAltersInPlace(t *testing.T) {
	f := setup(t)
	if _, err := f.store.CreateBranch(f.ctx, f.repo, "wider", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	want := append([]adapter.Column(nil), f.table.Columns...)
	var oldID int
	for i := range want {
		if want[i].Name == "price" {
			oldID = int(want[i].ID)
			want[i].SQLType = "numeric(18,2)"
		}
	}
	res, err := f.store.AlterBranchSchema(f.ctx, f.repo, f.table, "wider", want, principal)
	if err != nil {
		t.Fatalf("alter: %v", err)
	}
	if len(res.Forked) != 0 {
		t.Errorf("widening forked to a new column id: %v. Every stored value still "+
			"fits, so the column alters in place", res.Forked)
	}
	v, err := f.store.LoadSchema(f.ctx, f.repo, f.table, "wider")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Columns {
		if c.Name == "price" && int(c.ID) != oldID {
			t.Errorf("price changed id from %d to %d on a widening change", oldID, c.ID)
		}
	}
}
