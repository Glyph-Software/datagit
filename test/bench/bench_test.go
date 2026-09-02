// Package bench holds the performance regression gates (M4.6, §14.1).
//
// These are not benchmarks for their own sake. Each one asserts a BUDGET, and a
// build that blows the budget fails rather than quietly getting slower. That
// distinction matters: a benchmark nobody reads is a benchmark nobody acts on.
//
// The budgets are deliberately loose -- several times the measured figure --
// because they run on developer laptops and shared CI, and a gate that fails on
// noise gets disabled. They are set to catch a REGRESSION IN KIND: an index that
// stopped being used, a resolution that became quadratic in chain depth, a diff
// that started scanning the table. They are not a substitute for the §14.1
// measurements on representative hardware.
//
// Every gate runs on both engines, and the measured figures for each are printed
// so §14.1 can be populated from measurement rather than assumption (M5.3).
package bench

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/connect"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/db"
	"github.com/Glyph-Software/datagit/internal/store"
)

const principal = "bench@example.com"

// budget is one gate: a name, what it measures, and the ceiling.
type budget struct {
	name  string
	limit time.Duration
}

type harness struct {
	ctx     context.Context
	pool    db.Pool
	store   *store.Store
	repo    *store.Repo
	table   *store.Table
	dialect adapter.Dialect
	rows    int
}

func dsn() string {
	if v := os.Getenv("DATAGIT_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://datagit:datagit@localhost:55417/datagit"
}

func setup(t testing.TB, rows int) *harness {
	t.Helper()
	ctx := context.Background()
	base, ad, err := connect.Open(ctx, dsn())
	if err != nil {
		if os.Getenv("DATAGIT_TEST_DSN") != "" {
			t.Fatalf("DATAGIT_TEST_DSN names a database that is not reachable: %v", err)
		}
		t.Skipf("no database at %s: %v", dsn(), err)
	}
	t.Cleanup(base.Close)

	ns := fmt.Sprintf("it_bench_%d", time.Now().UnixNano())
	var scoped string
	if ad.Dialect() == adapter.MySQL {
		if err := base.Direct().Exec(ctx, "CREATE DATABASE "+ns); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = base.Direct().Exec(ctx, "DROP DATABASE IF EXISTS "+ns) })
		scoped = swapMySQLDatabase(dsn(), ns)
	} else {
		if err := base.Direct().Exec(ctx, "CREATE SCHEMA "+ns); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = base.Direct().Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
		scoped = dsn() + "?search_path=" + ns
	}
	pool, ad, err := connect.Open(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	st := store.New(pool, ad)
	if err := st.InitControlSchema(ctx); err != nil {
		t.Fatal(err)
	}
	idType := "text"
	if ad.Dialect() == adapter.MySQL {
		idType = "varchar(64)"
	}
	if err := pool.Direct().Exec(ctx,
		`CREATE TABLE items (id `+idType+` PRIMARY KEY, category varchar(32), price decimal(12,2))`); err != nil {
		t.Fatal(err)
	}
	// One multi-row insert: the fixture is setup cost, not the thing measured.
	var vals []string
	for i := 0; i < rows; i++ {
		vals = append(vals, fmt.Sprintf("('K%06d','cat%d',%d.00)", i, i%8, i%1000))
	}
	if err := pool.Direct().Exec(ctx,
		`INSERT INTO items VALUES `+strings.Join(vals, ",")); err != nil {
		t.Fatal(err)
	}

	repo, err := st.CreateRepo(ctx, "bench", principal)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := st.Track(ctx, repo, "items", adapter.ModeVersioned)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{ctx: ctx, pool: pool, store: st, repo: repo, table: tbl,
		dialect: ad.Dialect(), rows: rows}
}

func swapMySQLDatabase(dsn, name string) string {
	dsn = strings.TrimPrefix(dsn, "mysql://")
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn + "/" + name
	}
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	return dsn[:slash+1] + name + tail
}

// row builds the row for key i.
func (h *harness) row(i int) core.Row { return h.rowFor(i, i) }

// rowFor builds a row for key `key` with values derived from `n`, so one key can
// be updated repeatedly with a different value each time. The key column must
// match the primary key the change is filed under, or the write lands on a
// different row entirely.
func (h *harness) rowFor(key, n int) core.Row {
	c := h.table.Columns
	v, _ := core.Numeric(fmt.Sprintf("%d.50", n%1000))
	return core.Row{
		c[0].ID: core.Text(fmt.Sprintf("K%06d", key)),
		c[1].ID: core.Text(fmt.Sprintf("cat%d", n%8)),
		c[2].ID: v,
	}
}

func (h *harness) pk(i int) core.PK {
	return core.MakePK(core.Row{h.table.Columns[0].ID: core.Text(fmt.Sprintf("K%06d", i))},
		h.table.PKColumns)
}

// gate runs fn, reports the elapsed time, and fails if it exceeds the budget.
func gate(t *testing.T, h *harness, b budget, fn func() error) {
	t.Helper()
	start := time.Now()
	if err := fn(); err != nil {
		t.Fatalf("%s: %v", b.name, err)
	}
	took := time.Since(start)
	t.Logf("%-34s %-12s %8s  (budget %s)", b.name, h.dialect, took.Round(time.Millisecond), b.limit)
	if took > b.limit {
		t.Errorf("%s took %s on %s, over its %s budget. This gate exists to catch a "+
			"regression IN KIND -- an index that stopped being used, a resolution that "+
			"became quadratic -- so check the query plan before raising the budget",
			b.name, took.Round(time.Millisecond), h.dialect, b.limit)
	}
}

// TestGateBatchCommit: a batched commit is the write path §14.1 quotes rows/s
// for, because the ref lock caps commits/s regardless of batch size.
func TestGateBatchCommit(t *testing.T) {
	h := setup(t, 2000)
	changes := make([]store.Change, 0, 2000)
	for i := 0; i < 2000; i++ {
		changes = append(changes, store.Change{PK: h.pk(i), Op: core.OpUpdate, Row: h.row(i)})
	}
	gate(t, h, budget{"commit 2000 rows", 30 * time.Second}, func() error {
		_, err := h.store.Commit(h.ctx, store.CommitRequest{
			Repo: h.repo, Table: h.table, Branch: store.DefaultBranch,
			Author: principal, Message: "bulk", Changes: changes,
		})
		return err
	})
}

// TestGateBranchRead is the §7.3 resolution query: the one whose cost the whole
// storage design is a bet about.
func TestGateBranchRead(t *testing.T) {
	h := setup(t, 2000)
	if _, err := h.store.CreateBranch(h.ctx, h.repo, "b1", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	changes := make([]store.Change, 0, 200)
	for i := 0; i < 200; i++ {
		changes = append(changes, store.Change{PK: h.pk(i), Op: core.OpUpdate, Row: h.row(i)})
	}
	if _, err := h.store.Commit(h.ctx, store.CommitRequest{
		Repo: h.repo, Table: h.table, Branch: "b1",
		Author: principal, Message: "branch edits", Changes: changes,
	}); err != nil {
		t.Fatal(err)
	}

	gate(t, h, budget{"resolve a branch (2000 rows)", 15 * time.Second}, func() error {
		rows, err := h.store.Read(h.ctx, h.repo, h.table, store.ReadOptions{Branch: "b1"})
		if err != nil {
			return err
		}
		if len(rows) != h.rows {
			return fmt.Errorf("resolved %d rows, want %d", len(rows), h.rows)
		}
		return nil
	})
}

// TestGateFilteredBranchRead is the two-pass filtered read, and the shape that
// produced finding F7: without a per-column index this went from milliseconds to
// twenty seconds at scale.
func TestGateFilteredBranchRead(t *testing.T) {
	h := setup(t, 2000)
	if _, err := h.store.CreateBranch(h.ctx, h.repo, "b1", store.DefaultBranch, principal); err != nil {
		t.Fatal(err)
	}
	gate(t, h, budget{"filtered branch read", 10 * time.Second}, func() error {
		_, err := h.store.Read(h.ctx, h.repo, h.table, store.ReadOptions{
			Branch: "b1",
			Filter: adapter.Compare{Col: h.table.Columns[1].ID, Op: adapter.Eq,
				Value: core.Text("cat3")},
		})
		return err
	})
}

// TestGateDiffCostsTheSizeOfTheChange is §8.1's claim, stated as a gate: a diff
// of 50 changed rows in a 2000-row table must not cost the table.
func TestGateDiffCostsTheSizeOfTheChange(t *testing.T) {
	h := setup(t, 2000)
	changes := make([]store.Change, 0, 50)
	for i := 0; i < 50; i++ {
		changes = append(changes, store.Change{PK: h.pk(i), Op: core.OpUpdate, Row: h.row(i)})
	}
	if _, err := h.store.Commit(h.ctx, store.CommitRequest{
		Repo: h.repo, Table: h.table, Branch: store.DefaultBranch,
		Author: principal, Message: "small change", Changes: changes,
	}); err != nil {
		t.Fatal(err)
	}
	gate(t, h, budget{"diff 50 of 2000 rows", 5 * time.Second}, func() error {
		entries, err := h.store.Diff(h.ctx, h.repo, h.table, store.DefaultBranch, 0, 1)
		if err != nil {
			return err
		}
		if len(entries) != 50 {
			return fmt.Errorf("diff returned %d entries, want 50", len(entries))
		}
		return nil
	})
}

// TestGateHistoryAndBlame walks one key's version chain.
func TestGateHistoryAndBlame(t *testing.T) {
	h := setup(t, 200)
	for i := 0; i < 20; i++ {
		if _, err := h.store.Commit(h.ctx, store.CommitRequest{
			Repo: h.repo, Table: h.table, Branch: store.DefaultBranch,
			Author: principal, Message: fmt.Sprintf("v%d", i),
			Changes: []store.Change{{PK: h.pk(0), Op: core.OpUpdate, Row: h.rowFor(0, i+1)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	gate(t, h, budget{"history of one key (21 versions)", 3 * time.Second}, func() error {
		hist, err := h.store.History(h.ctx, h.repo, h.table, store.DefaultBranch, h.pk(0))
		if err != nil {
			return err
		}
		if len(hist) < 20 {
			return fmt.Errorf("history has %d versions, want at least 20", len(hist))
		}
		return nil
	})
	gate(t, h, budget{"blame one key", 3 * time.Second}, func() error {
		_, err := h.store.Blame(h.ctx, h.repo, h.table, store.DefaultBranch, h.pk(0), nil)
		return err
	})
}

// TestGateMergeAtDepth is the chain-depth bet: resolution walks a chain, and the
// §18 cap of 8 exists because the cost grows with it. A merge at the cap must
// still be fast.
func TestGateMergeAtDepth(t *testing.T) {
	h := setup(t, 500)
	parent := store.DefaultBranch
	for d := 1; d <= 7; d++ {
		name := fmt.Sprintf("d%d", d)
		if _, err := h.store.CreateBranch(h.ctx, h.repo, name, parent, principal); err != nil {
			t.Fatalf("branch at depth %d: %v", d, err)
		}
		if _, err := h.store.Commit(h.ctx, store.CommitRequest{
			Repo: h.repo, Table: h.table, Branch: name,
			Author: principal, Message: "edit",
			Changes: []store.Change{{PK: h.pk(d), Op: core.OpUpdate, Row: h.row(d)}},
		}); err != nil {
			t.Fatal(err)
		}
		parent = name
	}
	gate(t, h, budget{"resolve at chain depth 8", 15 * time.Second}, func() error {
		rows, err := h.store.Read(h.ctx, h.repo, h.table, store.ReadOptions{Branch: parent})
		if err != nil {
			return err
		}
		if len(rows) != h.rows {
			return fmt.Errorf("resolved %d rows at depth 8, want %d", len(rows), h.rows)
		}
		return nil
	})
}
