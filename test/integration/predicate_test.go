package integration

import (
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// TestUpdateWhereEqualsPerKeyUpdates is the guarantee that makes predicate
// writes safe (§7.4): "raise every outdoor price by 8%" must produce exactly the
// change set the equivalent per-key updates would.
func TestUpdateWhereEqualsPerKeyUpdates(t *testing.T) {
	f := setup(t)
	catCol, priceCol := f.table.Columns[2].ID, f.table.Columns[3].ID

	changes, err := f.store.PlanUpdateWhere(f.ctx, f.repo, f.table, store.DefaultBranch,
		eq(catCol, "outdoor"),
		[]store.Assignment{{Col: priceCol, Expr: store.Arith{
			Left:  store.ColRef{Col: priceCol},
			Op:    store.Mul,
			Right: store.Const{Value: core.MustNumeric("1.08")},
		}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(changes) != 2 { // TENT-4P and STOVE-V1 are outdoor
		t.Fatalf("plan produced %d changes, want 2", len(changes))
	}

	// Build the same change set by hand from the resolved rows.
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[core.PK]string{}
	for _, r := range rows {
		if r.Get(catCol).Text != "outdoor" {
			continue
		}
		switch r.Get(f.table.Columns[0].ID).Text {
		case "TENT-4P":
			want[core.MakePK(r, f.table.PKColumns)] = "268.92" // 249.00 * 1.08, exactly
		case "STOVE-V1":
			want[core.MakePK(r, f.table.PKColumns)] = "96.66" // 89.50 * 1.08, exactly
		}
	}
	for _, ch := range changes {
		got := ch.Row.Get(priceCol).Text
		if want[ch.PK] != got {
			t.Errorf("key %x: computed %s, want %s", ch.PK, got, want[ch.PK])
		}
	}
}

// TestUpdateWhereArithmeticIsExact: the result is hashed into history, so a
// rounding difference would make the same logical change produce a different
// commit id on a different machine.
func TestUpdateWhereArithmeticIsExact(t *testing.T) {
	f := setup(t)
	priceCol := f.table.Columns[3].ID

	changes, err := f.store.PlanUpdateWhere(f.ctx, f.repo, f.table, store.DefaultBranch,
		adapter.Compare{Col: f.table.Columns[0].ID, Op: adapter.Eq, Value: core.Text("STOVE-V1")},
		[]store.Assignment{{Col: priceCol, Expr: store.Arith{
			Left:  store.ColRef{Col: priceCol},
			Op:    store.Mul,
			Right: store.Const{Value: core.MustNumeric("1.08")},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	// 89.50 * 1.08 = 96.66 exactly. A float64 round trip gives 96.66000000000001.
	if got := changes[0].Row.Get(priceCol).Text; got != "96.66" {
		t.Errorf("89.50 * 1.08 computed as %s, want exactly 96.66", got)
	}
}

// TestUpdateWhereOnBranchUsesResolvedRows: the predicate must see the branch's
// state, not main's.
func TestUpdateWhereOnBranchUsesResolvedRows(t *testing.T) {
	f := setup(t)
	catCol, priceCol := f.table.Columns[2].ID, f.table.Columns[3].ID

	if _, err := f.store.CreateBranch(f.ctx, f.repo, "recat", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	// On the branch, the mug becomes outdoor. On main it stays kitchen.
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: "recat", Author: principal,
		Message: "recategorize the mug",
		Changes: []store.Change{{PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
			Row: f.row("MUG-01", "Enamel mug", "outdoor", "12.00", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}

	onBranch, err := f.store.PlanUpdateWhere(f.ctx, f.repo, f.table, "recat",
		eq(catCol, "outdoor"),
		[]store.Assignment{{Col: priceCol, Expr: store.Const{Value: core.MustNumeric("1.00")}}})
	if err != nil {
		t.Fatal(err)
	}
	onMain, err := f.store.PlanUpdateWhere(f.ctx, f.repo, f.table, store.DefaultBranch,
		eq(catCol, "outdoor"),
		[]store.Assignment{{Col: priceCol, Expr: store.Const{Value: core.MustNumeric("1.00")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(onBranch) != 3 {
		t.Errorf("on the branch the predicate matched %d rows, want 3", len(onBranch))
	}
	if len(onMain) != 2 {
		t.Errorf("on main the predicate matched %d rows, want 2", len(onMain))
	}
}

// TestValidateMergeCatchesUniqueViolation (M3.3, §9.3). A merge that would
// violate a real constraint must be caught BEFORE apply: merging into the
// default branch writes the live table, so the database would otherwise reject
// it mid-merge with a partial result.
func TestValidateMergeCatchesUniqueViolation(t *testing.T) {
	f := setup(t)
	f.boundName(t)
	must(t, f.pool.Direct().Exec(f.ctx,
		`CREATE UNIQUE INDEX products_name_uk ON products (name)`))

	// A change set that would give two rows the same name.
	changes := []store.Change{{
		PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
		Row: f.row("MUG-01", "Camp stove", "kitchen", "12.00", "2026-08-14T00:00:00Z"),
	}}
	vs, err := f.store.ValidateMerge(f.ctx, f.repo, f.table, store.DefaultBranch, changes)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(vs) == 0 {
		t.Fatal("a duplicate under a unique index was not caught before apply")
	}
	if vs[0].Kind != "unique" {
		t.Errorf("violation kind is %q, want unique", vs[0].Kind)
	}
}

// TestValidateMergeAllowsCleanChanges: validation must not cry wolf.
func TestValidateMergeAllowsCleanChanges(t *testing.T) {
	f := setup(t)
	f.boundName(t)
	must(t, f.pool.Direct().Exec(f.ctx,
		`CREATE UNIQUE INDEX products_name_uk2 ON products (name)`))
	changes := []store.Change{{
		PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
		Row: f.row("MUG-01", "Enamel mug XL", "kitchen", "14.00", "2026-08-14T00:00:00Z"),
	}}
	vs, err := f.store.ValidateMerge(f.ctx, f.repo, f.table, store.DefaultBranch, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Errorf("a clean change set reported %d violations: %v", len(vs), vs)
	}
}
