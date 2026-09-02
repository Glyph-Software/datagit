package integration

import (
	"fmt"
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/store"
)

// TestPartitionedSidecarBehavesIdentically is the first thing partitioning has
// to prove: it is a storage layout, not a behaviour change. Everything the
// unpartitioned sidecar does, the partitioned one does the same way.
func TestPartitionedSidecarBehavesIdentically(t *testing.T) {
	f := setup(t)

	idType := "text"
	if f.dialect == adapter.MySQL {
		idType = "varchar(64)"
	}
	must(t, f.pool.Direct().Exec(f.ctx, `
		CREATE TABLE readings (
			id  `+idType+` PRIMARY KEY,
			val decimal(12,2)
		)`))
	must(t, f.pool.Direct().Exec(f.ctx,
		`INSERT INTO readings VALUES ('R1', 1.00), ('R2', 2.00)`))

	tbl, err := f.store.TrackPartitioned(f.ctx, f.repo, "readings", adapter.ModeVersioned)
	if err != nil {
		t.Fatalf("track partitioned: %v", err)
	}

	pk := core.MakePK(core.Row{tbl.Columns[0].ID: core.Text("R1")}, tbl.PKColumns)
	for i := 2; i <= 5; i++ {
		if _, err := f.store.Commit(f.ctx, store.CommitRequest{
			Repo: f.repo, Table: tbl, Branch: store.DefaultBranch,
			Author: principal, Message: fmt.Sprintf("reading %d", i),
			Changes: []store.Change{{PK: pk, Op: core.OpUpdate, Row: core.Row{
				tbl.Columns[0].ID: core.Text("R1"),
				tbl.Columns[1].ID: mustNumeric(t, fmt.Sprintf("%d.00", i)),
			}}},
		}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	// Reads, history, and blame all work exactly as on an unpartitioned sidecar.
	rows, err := f.store.Read(f.ctx, f.repo, tbl, store.ReadOptions{Branch: store.DefaultBranch})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("read %d rows from a partitioned sidecar, want 2", len(rows))
	}
	hist, err := f.store.History(f.ctx, f.repo, tbl, store.DefaultBranch, pk)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 5 {
		t.Errorf("history has %d versions, want 5 (1 import + 4 commits)", len(hist))
	}
	if err := f.store.VerifyIntegrity(f.ctx, f.repo, store.DefaultBranch); err != nil {
		t.Errorf("a partitioned sidecar broke the hash chain: %v", err)
	}
}

// TestDropPartitionRemovesARange is what partitioning is FOR: pruning becomes a
// catalogue change instead of a scan.
func TestDropPartitionRemovesARange(t *testing.T) {
	f := setup(t)
	idType := "text"
	if f.dialect == adapter.MySQL {
		idType = "varchar(64)"
	}
	must(t, f.pool.Direct().Exec(f.ctx,
		`CREATE TABLE readings (id `+idType+` PRIMARY KEY, val decimal(12,2))`))
	must(t, f.pool.Direct().Exec(f.ctx, `INSERT INTO readings VALUES ('R1', 1.00)`))

	tbl, err := f.store.TrackPartitioned(f.ctx, f.repo, "readings", adapter.ModeVersioned)
	if err != nil {
		t.Fatal(err)
	}
	// Partitions are declared AHEAD of the sequences they will hold. PostgreSQL
	// will not carve a range out from under the default partition once rows are
	// already there, and the error says so.
	if err := f.store.AddPartition(f.ctx, f.repo, tbl, store.DefaultBranch, 1, 4); err != nil {
		t.Fatalf("add partition: %v", err)
	}

	pk := core.MakePK(core.Row{tbl.Columns[0].ID: core.Text("R1")}, tbl.PKColumns)
	for i := 2; i <= 6; i++ {
		if _, err := f.store.Commit(f.ctx, store.CommitRequest{
			Repo: f.repo, Table: tbl, Branch: store.DefaultBranch,
			Author: principal, Message: fmt.Sprintf("v%d", i),
			Changes: []store.Change{{PK: pk, Op: core.OpUpdate, Row: core.Row{
				tbl.Columns[0].ID: core.Text("R1"),
				tbl.Columns[1].ID: mustNumeric(t, fmt.Sprintf("%d.00", i)),
			}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	before := f.sidecarRowsIn(t, "readings")

	if err := f.store.DropPartition(f.ctx, f.repo, tbl, store.DefaultBranch, 1, 4); err != nil {
		t.Fatalf("drop partition: %v", err)
	}
	after := f.sidecarRowsIn(t, "readings")
	if after >= before {
		t.Errorf("dropping a partition removed nothing: %d rows before, %d after",
			before, after)
	}
	// The current state survives: the dropped range held old versions, and the
	// open version lives past it.
	rows, err := f.store.Read(f.ctx, f.repo, tbl, store.ReadOptions{Branch: store.DefaultBranch})
	if err != nil {
		t.Fatalf("read after dropping a partition: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("dropping an old partition lost the current row: %d rows", len(rows))
	}
}

func mustNumeric(t *testing.T, s string) core.Value {
	t.Helper()
	v, err := core.Numeric(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// sidecarRowsIn counts versions in a named table's sidecar.
func (f *fixture) sidecarRowsIn(t *testing.T, physical string) int {
	t.Helper()
	var n int
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT count(*) FROM "datagit_v_`+physical+`"`).Scan(&n))
	return n
}
