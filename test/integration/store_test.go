// Package integration exercises the M1 foundation against a real PostgreSQL.
//
// Run with:
//
//	make db-up && make test-integration
//
// Skipped when no database is reachable, so `go test ./...` stays green without
// Docker.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/pg"
	"github.com/Glyph-Software/datagit/internal/store"
)

const principal = "test@example.com"

func dsn() string {
	if v := os.Getenv("DATAGIT_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://datagit:datagit@localhost:55417/datagit"
}

type fixture struct {
	ctx   context.Context
	pool  *pg.Pool
	store *store.Store
	repo  *store.Repo
	table *store.Table
}

// setup builds an isolated schema per test, so tests neither collide nor depend
// on order.
func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pg.Open(ctx, dsn())
	if err != nil {
		t.Skipf("no database at %s: %v", dsn(), err)
	}
	t.Cleanup(pool.Close)

	schema := fmt.Sprintf("it_%d", time.Now().UnixNano())
	must(t, pool.Direct().Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema)))
	t.Cleanup(func() {
		_ = pool.Direct().Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
	})
	must(t, pool.Direct().Exec(ctx, fmt.Sprintf(`SET search_path TO %s`, schema)))

	// A fresh pool so every connection lands in the test's schema.
	pool2, err := pg.Open(ctx, dsn()+"?search_path="+schema)
	must(t, err)
	t.Cleanup(pool2.Close)

	st := store.New(pool2, postgres.New())
	must(t, st.InitControlSchema(ctx))
	must(t, st.CheckControlSchema(ctx))

	must(t, pool2.Direct().Exec(ctx, `
		CREATE TABLE products (
			sku        text PRIMARY KEY,
			name       text,
			category   text,
			price      numeric(12,2),
			updated_at timestamptz
		)`))
	must(t, pool2.Direct().Exec(ctx, `
		INSERT INTO products VALUES
			('TENT-4P', 'Four-person tent', 'outdoor', 249.00, '2026-03-02T00:00:00Z'),
			('STOVE-V1','Camp stove',       'outdoor',  89.50, '2026-03-02T00:00:00Z'),
			('MUG-01',  'Enamel mug',       'kitchen',  12.00, '2026-03-02T00:00:00Z')`))

	repo, err := st.CreateRepo(ctx, "catalog", principal)
	must(t, err)
	tbl, err := st.Track(ctx, repo, "products", adapter.ModeVersioned)
	must(t, err)

	return &fixture{ctx: ctx, pool: pool2, store: st, repo: repo, table: tbl}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func (f *fixture) pk(t *testing.T, sku string) core.PK {
	t.Helper()
	return core.MakePK(core.Row{f.table.PKColumns[0]: core.Text(sku)}, f.table.PKColumns)
}

func (f *fixture) row(sku, name, category, price, at string) core.Row {
	cols := f.table.Columns
	ts, _ := time.Parse(time.RFC3339, at)
	return core.Row{
		cols[0].ID: core.Text(sku),
		cols[1].ID: core.Text(name),
		cols[2].ID: core.Text(category),
		cols[3].ID: core.MustNumeric(price),
		cols[4].ID: core.Time(ts),
	}
}

func (f *fixture) liveCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.Direct().QueryRow(f.ctx, `SELECT count(*) FROM products`).Scan(&n); err != nil {
		t.Fatalf("live count: %v", err)
	}
	return n
}

func (f *fixture) livePrice(t *testing.T, sku string) string {
	t.Helper()
	var s string
	if err := f.pool.Direct().QueryRow(f.ctx,
		`SELECT price::text FROM products WHERE sku=$1`, sku).Scan(&s); err != nil {
		t.Fatalf("live price: %v", err)
	}
	return s
}

// TestTrackBackfillsAndLeavesLiveTableAlone is the load-bearing invariant:
// tracking must not modify the application's table (DESIGN.md §5.1).
func TestTrackBackfillsAndLeavesLiveTableAlone(t *testing.T) {
	f := setup(t)

	if got := f.liveCount(t); got != 3 {
		t.Fatalf("tracking changed the live row count: got %d, want 3", got)
	}
	// No added columns, no triggers: the live table must stay a clean
	// materialization of main@HEAD.
	var extra int
	must(t, f.pool.Direct().QueryRow(f.ctx, `
		SELECT count(*) FROM pg_attribute a
		  JOIN pg_class c ON c.oid=a.attrelid
		  JOIN pg_namespace n ON n.oid=c.relnamespace
		 WHERE c.relname='products' AND n.nspname=current_schema()
		   AND a.attnum>0 AND NOT a.attisdropped`).Scan(&extra))
	if extra != 5 {
		t.Errorf("tracking added columns to the live table: %d columns, want 5", extra)
	}
	var triggers int
	must(t, f.pool.Direct().QueryRow(f.ctx, `
		SELECT count(*) FROM pg_trigger tg
		  JOIN pg_class c ON c.oid=tg.tgrelid
		  JOIN pg_namespace n ON n.oid=c.relnamespace
		 WHERE c.relname='products' AND n.nspname=current_schema()
		   AND NOT tg.tgisinternal`).Scan(&triggers))
	if triggers != 0 {
		t.Errorf("tracking added %d triggers to the live table; the happy path must add none", triggers)
	}

	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("backfill resolved %d rows, want 3", len(rows))
	}
}

// TestCommitIsAtomicAcrossLiveTableAndHistory is DESIGN.md §6.1: the live write,
// the version record, and the commit land together.
func TestCommitIsAtomicAcrossLiveTableAndHistory(t *testing.T) {
	f := setup(t)

	res, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch,
		Author: principal, Message: "Q4 outdoor price increase", ExternalRef: "FIN-2291",
		Changes: []store.Change{{
			PK:  f.pk(t, "TENT-4P"),
			Op:  core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "outdoor", "268.92", "2026-08-14T00:00:00Z"),
		}},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Changed != 1 {
		t.Errorf("commit changed %d rows, want 1", res.Changed)
	}

	// The live table IS the new state, immediately, for direct readers.
	if got := f.livePrice(t, "TENT-4P"); got != "268.92" {
		t.Errorf("live table price is %s, want 268.92", got)
	}
	// And the resolved state agrees.
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text == "TENT-4P" {
			if got := r.Get(f.table.Columns[3].ID).Text; got != "268.92" {
				t.Errorf("resolved price is %s, want 268.92", got)
			}
		}
	}
}

// TestCommitRequiresAuthenticatedAuthor: DESIGN.md §15.2. An audit trail whose
// author can be supplied by the client is decoration.
func TestCommitRequiresAuthenticatedAuthor(t *testing.T) {
	f := setup(t)
	_, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Message: "no author",
	})
	if err == nil {
		t.Fatal("commit without an author must be refused")
	}
}

// TestNoOpWriteRecordsNothing: writing a row's current value is not a change and
// must not create a version, or history would fill with noise that blame then
// has to filter out.
func TestNoOpWriteRecordsNothing(t *testing.T) {
	f := setup(t)
	res, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "rewrite the same values",
		Changes: []store.Change{{
			PK:  f.pk(t, "MUG-01"),
			Op:  core.OpUpdate,
			Row: f.row("MUG-01", "Enamel mug", "kitchen", "12.00", "2026-03-02T00:00:00Z"),
		}},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Changed != 0 {
		t.Errorf("a no-op write recorded %d changes, want 0", res.Changed)
	}
}

// TestTimeTravel reads a past commit and a past timestamp (§7.2).
func TestTimeTravel(t *testing.T) {
	f := setup(t)

	before, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "first change",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "outdoor", "255.00", "2026-05-01T00:00:00Z")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	mid := time.Now()
	time.Sleep(10 * time.Millisecond)

	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "second change",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "outdoor", "268.92", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}

	priceAt := func(opt store.ReadOptions) string {
		rows, err := f.store.Read(f.ctx, f.repo, f.table, opt)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		for _, r := range rows {
			if r.Get(f.table.Columns[0].ID).Text == "TENT-4P" {
				return r.Get(f.table.Columns[3].ID).Text
			}
		}
		return "(absent)"
	}

	if got := priceAt(store.ReadOptions{}); got != "268.92" {
		t.Errorf("head price is %s, want 268.92", got)
	}
	// The canonical form strips trailing zeros: 255.00 and 255 are the same
	// number, so they must encode identically (§12.1). The live table still
	// renders numeric(12,2) as "255.00"; both are correct spellings.
	if got := priceAt(store.ReadOptions{At: &before.ID}); got != "255" {
		t.Errorf("price at the first commit is %s, want 255", got)
	}
	if got := priceAt(store.ReadOptions{AsOf: &mid}); got != "255" {
		t.Errorf("price as of a timestamp between the commits is %s, want 255", got)
	}
}

// TestDeleteDoesNotResurface is the §7.3 tombstone hazard, at the store level.
func TestDeleteDoesNotResurface(t *testing.T) {
	f := setup(t)
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "discontinue the stove",
		Changes: []store.Change{{PK: f.pk(t, "STOVE-V1"), Op: core.OpDelete}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.liveCount(t); got != 2 {
		t.Errorf("live row count is %d after a delete, want 2", got)
	}
	rows, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Get(f.table.Columns[0].ID).Text == "STOVE-V1" {
			t.Fatal("a deleted row resurfaced through resolution (§7.3)")
		}
	}
	if len(rows) != 2 {
		t.Errorf("resolution returned %d rows after a delete, want 2", len(rows))
	}
}

// TestHistoryAndBlame checks per-cell attribution (M1.6).
func TestHistoryAndBlame(t *testing.T) {
	f := setup(t)
	commits := []struct{ price, msg string }{
		{"255.00", "spring adjustment"},
		{"268.92", "Q4 outdoor price increase"},
	}
	for _, c := range commits {
		if _, err := f.store.Commit(f.ctx, store.CommitRequest{
			Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
			Message: c.msg,
			Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
				Row: f.row("TENT-4P", "Four-person tent", "outdoor", c.price, "2026-08-14T00:00:00Z")}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	hist, err := f.store.History(f.ctx, f.repo, f.table, store.DefaultBranch, f.pk(t, "TENT-4P"))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 { // import + two commits
		t.Errorf("history has %d versions, want 3", len(hist))
	}

	blame, err := f.store.Blame(f.ctx, f.repo, f.table, store.DefaultBranch,
		f.pk(t, "TENT-4P"), []core.ColID{f.table.Columns[3].ID})
	if err != nil {
		t.Fatalf("blame: %v", err)
	}
	if len(blame) != 1 {
		t.Fatalf("blame returned %d cells, want 1", len(blame))
	}
	if blame[0].Value.Text != "268.92" {
		t.Errorf("blamed value is %s, want 268.92", blame[0].Value)
	}
	if blame[0].Message != "Q4 outdoor price increase" {
		t.Errorf("blamed message is %q, want the last price change", blame[0].Message)
	}

	// The name column never changed, so it must be attributed to the import,
	// not to the latest commit that happened to rewrite the row.
	nameBlame, err := f.store.Blame(f.ctx, f.repo, f.table, store.DefaultBranch,
		f.pk(t, "TENT-4P"), []core.ColID{f.table.Columns[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if nameBlame[0].Message == "Q4 outdoor price increase" {
		t.Error("an unchanged column was attributed to the latest commit; blame must find where the value actually changed")
	}
}

// TestDiffCostsTheChange checks the two-point diff (M1.7, §8.1).
func TestDiffCostsTheChange(t *testing.T) {
	f := setup(t)
	first, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "one row",
		Changes: []store.Change{{PK: f.pk(t, "TENT-4P"), Op: core.OpUpdate,
			Row: f.row("TENT-4P", "Four-person tent", "outdoor", "268.92", "2026-08-14T00:00:00Z")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := f.store.Diff(f.ctx, f.repo, f.table, store.DefaultBranch, first.Seq-1, first.Seq)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("diff returned %d entries, want 1 (it must cost the change, not the table)", len(entries))
	}
	if entries[0].Op != core.OpUpdate {
		t.Errorf("diff op is %s, want update", entries[0].Op)
	}
	// Only the changed cells should be marked.
	if n := entries[0].Changed.Count(); n != 2 { // price and updated_at
		t.Errorf("diff marked %d changed columns, want 2", n)
	}
}

// TestIntegrityChainVerifies recomputes every commit hash (M1.11, §17.3).
func TestIntegrityChainVerifies(t *testing.T) {
	f := setup(t)
	for i := 0; i < 3; i++ {
		if _, err := f.store.Commit(f.ctx, store.CommitRequest{
			Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
			Message: fmt.Sprintf("change %d", i),
			Changes: []store.Change{{PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
				Row: f.row("MUG-01", "Enamel mug", "kitchen",
					fmt.Sprintf("%d.50", 13+i), "2026-08-14T00:00:00Z")}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.VerifyIntegrity(f.ctx, f.repo, store.DefaultBranch); err != nil {
		t.Errorf("hash chain does not verify: %v", err)
	}
}

// TestIntegrityDetectsTampering: the chain is tamper-EVIDENT. Anyone with write
// access to the database can rewrite both the data and the record (§12.2), but
// they cannot do so undetectably without also recomputing the hash.
func TestIntegrityDetectsTampering(t *testing.T) {
	f := setup(t)
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "original message",
		Changes: []store.Change{{PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
			Row: f.row("MUG-01", "Enamel mug", "kitchen", "13.50", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}
	must(t, f.pool.Direct().Exec(f.ctx,
		`UPDATE datagit_commit SET message='forged' WHERE message='original message'`))

	if err := f.store.VerifyIntegrity(f.ctx, f.repo, store.DefaultBranch); err == nil {
		t.Error("tampering with a commit message went undetected")
	}
}

// TestDriftDetection catches out-of-band writes (M1.11, §6.3). In `open` mode
// they are possible and undetected until a scan; this is that scan.
func TestDriftDetection(t *testing.T) {
	f := setup(t)
	rep, err := f.store.VerifyDrift(f.ctx, f.repo, f.table)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if rep.LiveOnly != 0 || rep.VersionOnly != 0 || rep.Mismatched != 0 {
		t.Fatalf("clean repository reports drift: %+v", rep)
	}

	// An out-of-band write, exactly what a psql session or a legacy job does.
	must(t, f.pool.Direct().Exec(f.ctx,
		`UPDATE products SET price = 999.99 WHERE sku = 'MUG-01'`))

	rep, err = f.store.VerifyDrift(f.ctx, f.repo, f.table)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mismatched != 1 {
		t.Errorf("out-of-band write not detected: %+v", rep)
	}
}

// TestFilteredReadMatchesUnfiltered is the §7.3 two-pass guarantee at the store
// level: a filtered read must equal the filter applied to the full resolution.
func TestFilteredReadMatchesUnfiltered(t *testing.T) {
	f := setup(t)
	// Move a row out of the category being filtered on. This is precisely the
	// case that breaks if the predicate is pushed into the resolution arms.
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "recategorize the stove",
		Changes: []store.Change{{PK: f.pk(t, "STOVE-V1"), Op: core.OpUpdate,
			Row: f.row("STOVE-V1", "Camp stove", "kitchen", "89.50", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}

	catCol := f.table.Columns[2].ID
	filtered, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{
		Filter: adapter.Compare{Col: catCol, Op: adapter.Eq, Value: core.Text("outdoor")},
	})
	if err != nil {
		t.Fatalf("filtered read: %v", err)
	}
	all, err := f.store.Read(f.ctx, f.repo, f.table, store.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, r := range all {
		if r.Get(catCol).Text == "outdoor" {
			want++
		}
	}
	if len(filtered) != want {
		t.Errorf("filtered read returned %d rows, want %d (the filter applied to the full resolution)",
			len(filtered), want)
	}
	for _, r := range filtered {
		if r.Get(catCol).Text != "outdoor" {
			t.Errorf("filtered read returned a row in category %q: a stale version resurfaced (§7.3)",
				r.Get(catCol).Text)
		}
	}
}

// TestUntrackLeavesTheLiveTable is the exit door (§17.5). Adoption must not be a
// one-way door.
func TestUntrackLeavesTheLiveTable(t *testing.T) {
	f := setup(t)
	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch, Author: principal,
		Message: "a change",
		Changes: []store.Change{{PK: f.pk(t, "MUG-01"), Op: core.OpUpdate,
			Row: f.row("MUG-01", "Enamel mug", "kitchen", "14.00", "2026-08-14T00:00:00Z")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Untrack(f.ctx, f.repo, f.table); err != nil {
		t.Fatalf("untrack: %v", err)
	}
	if got := f.liveCount(t); got != 3 {
		t.Errorf("untrack changed the live row count: got %d, want 3", got)
	}
	if got := f.livePrice(t, "MUG-01"); got != "14.00" {
		t.Errorf("untrack lost a committed value: price is %s, want 14.00", got)
	}
	var n int
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE c.relname='datagit_v_products' AND n.nspname=current_schema()`).Scan(&n))
	if n != 0 {
		t.Error("untrack left the sidecar behind")
	}
}

// TestTrackRefusesTableWithoutPrimaryKey: DESIGN.md §3.2. A clear refusal beats
// a surrogate identity with degraded merge semantics.
func TestTrackRefusesTableWithoutPrimaryKey(t *testing.T) {
	f := setup(t)
	must(t, f.pool.Direct().Exec(f.ctx, `CREATE TABLE events (id bigint, payload text)`))
	_, err := f.store.Track(f.ctx, f.repo, "events", adapter.ModeVersioned)
	if err == nil {
		t.Fatal("tracking a table with no primary key must be refused")
	}
	if !contains(err.Error(), "primary key") {
		t.Errorf("refusal should name the reason, got: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
