package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/schemaeng"
	"github.com/Glyph-Software/datagit/internal/store"
)

// TestMigrationResumesFromTheJournal is §10.4 against a real database, and the
// behaviour S4 proved converges from every crash point.
func TestMigrationResumesFromTheJournal(t *testing.T) {
	f := setup(t)

	// An additive plan: add two columns.
	base := &schemaeng.Version{TableID: uint64(f.table.ID), Columns: f.table.Columns, PK: f.table.PKColumns}
	next := &schemaeng.Version{TableID: uint64(f.table.ID), PK: f.table.PKColumns,
		Columns: append(append([]adapter.Column{}, f.table.Columns...),
			adapter.Column{ID: 90, Name: "margin_pct", SQLType: "numeric(5,2)", Nullable: true},
			adapter.Column{ID: 91, Name: "supplier", SQLType: "text", Nullable: true})}

	plan := schemaeng.Plan(uint64(f.table.ID), "products", schemaeng.Diff(base, next))
	if len(plan.Ops) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(plan.Ops))
	}

	ad := postgres.NewWithExec(func(ctx context.Context, sql string) error {
		return f.store.Exec(ctx, sql)
	})
	j := f.store.Journal()

	// Apply the first operation only, then simulate a crash by stopping.
	partial := &adapter.MigrationPlan{TableID: plan.TableID, Ops: plan.Ops[:1]}
	if err := ad.ApplyMigration(f.ctx, partial, j); err != nil {
		t.Fatalf("partial apply: %v", err)
	}
	if !f.columnExists(t, "margin_pct") {
		t.Fatal("the first operation did not apply")
	}
	if f.columnExists(t, "supplier") {
		t.Fatal("the second operation applied when it should not have")
	}

	// Restart with the FULL plan. It must resume, not redo: re-running the first
	// operation would fail without idempotency.
	if err := ad.ApplyMigration(f.ctx, plan, j); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !f.columnExists(t, "margin_pct") || !f.columnExists(t, "supplier") {
		t.Fatal("the resumed apply did not converge")
	}

	// And a third apply is a no-op, because everything is journalled complete.
	if err := ad.ApplyMigration(f.ctx, plan, j); err != nil {
		t.Fatalf("re-apply of a complete plan must be a no-op, got: %v", err)
	}
}

// TestMigrationOperationsAreIdempotent: a resume re-runs whatever was in flight
// when the process died, so every operation must tolerate being run twice.
func TestMigrationOperationsAreIdempotent(t *testing.T) {
	f := setup(t)
	base := &schemaeng.Version{TableID: uint64(f.table.ID), Columns: f.table.Columns, PK: f.table.PKColumns}
	next := &schemaeng.Version{TableID: uint64(f.table.ID), PK: f.table.PKColumns,
		Columns: append(append([]adapter.Column{}, f.table.Columns...),
			adapter.Column{ID: 92, Name: "notes", SQLType: "text", Nullable: true})}
	plan := schemaeng.Plan(uint64(f.table.ID), "products", schemaeng.Diff(base, next))

	ad := postgres.NewWithExec(func(ctx context.Context, sql string) error {
		return f.store.Exec(ctx, sql)
	})
	j := f.store.Journal()
	if err := ad.ApplyMigration(f.ctx, plan, j); err != nil {
		t.Fatal(err)
	}

	// Clear the completion marks and re-run: this is exactly the mid-step crash
	// case, where the journal says started but never completed.
	must(t, f.pool.Direct().Exec(f.ctx,
		`UPDATE datagit_migration_journal SET completed_at = NULL`))
	if err := ad.ApplyMigration(f.ctx, plan, j); err != nil {
		t.Fatalf("re-running an in-flight operation must be idempotent, got: %v", err)
	}
	if !f.columnExists(t, "notes") {
		t.Error("the column vanished after an idempotent re-run")
	}
}

func (f *fixture) columnExists(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := f.pool.Direct().QueryRow(f.ctx, fmt.Sprintf(`
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = 'products'
		   AND column_name = '%s'`, name)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

var _ = store.DefaultBranch
