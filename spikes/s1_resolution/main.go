// S1: branch-resolution performance and correctness in PostgreSQL.
// THROWAWAY spike code (PLAN.md Phase 0) — not part of the shipped tree.
//
//	go run ./spikes/s1_resolution -mode correctness
//	go run ./spikes/s1_resolution -mode bench
//	go run ./spikes/s1_resolution -mode explain
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	mainSeq   = 1000 // main's head seq in the synthetic data
	branchSeq = 5    // every synthetic branch's head/fork seq
)

var (
	flagDSN  = flag.String("dsn", "postgres://datagit:datagit@localhost:55417/datagit", "postgres DSN")
	flagMode = flag.String("mode", "bench", "correctness | bench | explain")
	flagIter = flag.Int("iter", 500, "iterations per latency measurement")
	flagKeys = flag.Int("keys", 10000000, "live key count in the dataset")
)

type segment struct {
	branch int
	seq    int64
}

func bid(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
}

// chain builds the priority-ordered segment chain for a given depth.
// depth 1 = main only; depth 3 = b2 -> b1 -> main; depth 8 = b7 -> ... -> main.
func chain(depth int) []segment {
	if depth == 1 {
		return []segment{{0, mainSeq}}
	}
	var segs []segment
	for b := depth - 1; b >= 1; b-- {
		segs = append(segs, segment{b, branchSeq})
	}
	return append(segs, segment{0, mainSeq})
}

const cols = "sku, name, category, price"

// resolveSQL is the CORRECT resolution form (DESIGN.md §7.3): tombstones are
// filtered in the OUTER scope, after the winner is chosen.
func resolveSQL(segs []segment, extraArm string) string {
	var arms []string
	for i, s := range segs {
		arms = append(arms, fmt.Sprintf(
			`SELECT %d AS prio, v.sku, v.name, v.category, v.price, v.op
			   FROM datagit_v_products v
			  WHERE v.branch_id = '%s' AND v.session_id IS NULL
			    AND v.seq_from <= %d AND v.seq_to > %d %s`,
			i, bid(s.branch), s.seq, s.seq, extraArm))
	}
	return fmt.Sprintf(`
		SELECT %s FROM (
			SELECT DISTINCT ON (sku) sku, name, category, price, op
			FROM (%s) s
			ORDER BY sku, prio
		) r
		WHERE r.op <> 3`, cols, strings.Join(arms, "\nUNION ALL\n"))
}

// resolveWrongTombstone is the BUG: `op <> 3` inside each arm. A branch-level
// delete is dropped from its own arm, so the parent's older version wins and the
// deleted row resurfaces.
func resolveWrongTombstone(segs []segment) string {
	var arms []string
	for i, s := range segs {
		arms = append(arms, fmt.Sprintf(
			`SELECT %d AS prio, v.sku, v.name, v.category, v.price, v.op
			   FROM datagit_v_products v
			  WHERE v.branch_id = '%s' AND v.session_id IS NULL
			    AND v.seq_from <= %d AND v.seq_to > %d AND v.op <> 3`,
			i, bid(s.branch), s.seq, s.seq))
	}
	return fmt.Sprintf(`
		SELECT %s FROM (
			SELECT DISTINCT ON (sku) sku, name, category, price, op
			FROM (%s) s ORDER BY sku, prio
		) r`, cols, strings.Join(arms, "\nUNION ALL\n"))
}

// filteredTwoPass is the CORRECT filtered read (DESIGN.md §7.3): candidate keys
// from any segment, then full resolution of exactly those keys, then the filter
// applied to the winner.
func filteredTwoPass(segs []segment, category string) string {
	var candArms, resArms []string
	for i, s := range segs {
		candArms = append(candArms, fmt.Sprintf(
			`SELECT v.sku FROM datagit_v_products v
			  WHERE v.branch_id = '%s' AND v.session_id IS NULL
			    AND v.seq_from <= %d AND v.seq_to > %d
			    AND v.op <> 3 AND v.category = '%s'`,
			bid(s.branch), s.seq, s.seq, category))
		resArms = append(resArms, fmt.Sprintf(
			`SELECT %d AS prio, v.sku, v.name, v.category, v.price, v.op
			   FROM datagit_v_products v JOIN cand USING (sku)
			  WHERE v.branch_id = '%s' AND v.session_id IS NULL
			    AND v.seq_from <= %d AND v.seq_to > %d`,
			i, bid(s.branch), s.seq, s.seq))
	}
	return fmt.Sprintf(`
		WITH cand AS (
			SELECT DISTINCT sku FROM (%s) k
		)
		SELECT %s FROM (
			SELECT DISTINCT ON (s.sku) s.sku, s.name, s.category, s.price, s.op
			FROM (%s) s ORDER BY s.sku, s.prio
		) r
		WHERE r.op <> 3 AND r.category = '%s'`,
		strings.Join(candArms, "\nUNION ALL\n"), cols,
		strings.Join(resArms, "\nUNION ALL\n"), category)
}

// filteredTwoPassPaged is the two-pass form with the page pushed into pass 1.
//
// This is the shape the structured read API actually issues: Scan takes a
// cursor and a limit (§7.4). Bounding the candidate set turns pass 2 from a
// scatter-gather over every matching key into a handful of index lookups.
//
// Note the over-fetch. Pass 2 re-applies the predicate to the resolved winner
// and may discard candidates, so taking exactly `limit` candidates could return
// a short page. The API must over-fetch and continue from the cursor until the
// page is full.
func filteredTwoPassPaged(segs []segment, category string, after string, limit int) string {
	over := limit * 2
	var candArms, resArms []string
	for i, s := range segs {
		// Each arm is ordered and limited INDIVIDUALLY before the union.
		//
		// Without this, `SELECT DISTINCT sku FROM (union) ORDER BY sku LIMIT n`
		// must aggregate the entire union before it can sort and limit, so the
		// page bounds the output but not the work. Per-arm ordering lets each
		// arm stop early — but only if the index's trailing column is the
		// primary key, so that the scan is already in sku order.
		candArms = append(candArms, fmt.Sprintf(
			`(SELECT v.sku FROM datagit_v_products v
			   WHERE v.branch_id = '%s' AND v.session_id IS NULL
			     AND v.seq_from <= %d AND v.seq_to > %d
			     AND v.op <> 3 AND v.category = '%s' AND v.sku > '%s'
			   ORDER BY v.sku LIMIT %d)`,
			bid(s.branch), s.seq, s.seq, category, after, over))
		resArms = append(resArms, fmt.Sprintf(
			`SELECT %d AS prio, v.sku, v.name, v.category, v.price, v.op
			   FROM datagit_v_products v JOIN cand USING (sku)
			  WHERE v.branch_id = '%s' AND v.session_id IS NULL
			    AND v.seq_from <= %d AND v.seq_to > %d`,
			i, bid(s.branch), s.seq, s.seq))
	}
	return fmt.Sprintf(`
		WITH cand AS (
			SELECT sku FROM (SELECT DISTINCT sku FROM (%s) k ORDER BY sku LIMIT %d) c
		)
		SELECT %s FROM (
			SELECT DISTINCT ON (s.sku) s.sku, s.name, s.category, s.price, s.op
			FROM (%s) s ORDER BY s.sku, s.prio
		) r
		WHERE r.op <> 3 AND r.category = '%s'
		ORDER BY r.sku LIMIT %d`,
		strings.Join(candArms, "\nUNION ALL\n"), over, cols,
		strings.Join(resArms, "\nUNION ALL\n"), category, limit)
}

// filteredWrongPushdown is the BUG: the predicate is evaluated inside each arm
// and never re-checked against the resolved winner. A row the branch edited out
// of the category reappears from the parent, still carrying the old category.
func filteredWrongPushdown(segs []segment, category string) string {
	return resolveSQL(segs, fmt.Sprintf("AND v.category = '%s'", category))
}

// filteredNaive resolves the whole table and then filters — correct but O(table).
// It is the reference the two-pass form must match.
func filteredNaive(segs []segment, category string) string {
	return fmt.Sprintf(`SELECT %s FROM (%s) x WHERE x.category = '%s'`,
		cols, resolveSQL(segs, ""), category)
}

type row struct {
	sku, name, category string
	price               string
}

func query(ctx context.Context, pool *pgxpool.Pool, sql string) ([]row, error) {
	rs, err := pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.sku, &r.name, &r.category, &r.price); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func skuSet(rows []row) map[string]row {
	m := make(map[string]row, len(rows))
	for _, r := range rows {
		m[r.sku] = r
	}
	return m
}

// --- correctness: the two §7.3 hazards ---

func correctness(ctx context.Context, pool *pgxpool.Pool) int {
	fail := 0
	fmt.Println("=== S1 correctness: the two §7.3 resolution hazards")

	for _, depth := range []int{3, 8} {
		segs := chain(depth)

		// Hazard 1: tombstone fallthrough.
		good, err := query(ctx, pool, resolveSQL(segs, ""))
		must(err)
		bad, err := query(ctx, pool, resolveWrongTombstone(segs))
		must(err)
		g, b := skuSet(good), skuSet(bad)
		var resurfaced int
		for sku := range b {
			if _, ok := g[sku]; !ok {
				resurfaced++
			}
		}
		fmt.Printf("\n[depth %d] hazard 1 — `op <> 3` placement\n", depth)
		fmt.Printf("  correct (filter outside arms): %d live rows\n", len(good))
		fmt.Printf("  buggy   (filter inside arms):  %d live rows\n", len(bad))
		fmt.Printf("  rows resurfaced by the bug:    %d\n", resurfaced)
		if resurfaced == 0 {
			fmt.Println("  FAIL: the hazard did not reproduce — the dataset lacks branch tombstones")
			fail++
		} else {
			fmt.Println("  OK: hazard reproduces, correct form avoids it")
		}

		// Hazard 2: predicate pushdown into the resolution arms.
		const cat = "cat-0007"
		ref, err := query(ctx, pool, filteredNaive(segs, cat))
		must(err)
		two, err := query(ctx, pool, filteredTwoPass(segs, cat))
		must(err)
		push, err := query(ctx, pool, filteredWrongPushdown(segs, cat))
		must(err)

		fmt.Printf("\n[depth %d] hazard 2 — filter pushed into arms (category = %s)\n", depth, cat)
		fmt.Printf("  reference (resolve then filter): %d rows\n", len(ref))
		fmt.Printf("  two-pass:                        %d rows\n", len(two))
		fmt.Printf("  buggy pushdown:                  %d rows\n", len(push))

		if !sameRows(ref, two) {
			fmt.Println("  FAIL: two-pass disagrees with the reference")
			fail++
		} else {
			fmt.Println("  OK: two-pass matches the reference exactly")
		}
		if sameRows(ref, push) {
			fmt.Println("  FAIL: pushdown did not diverge — dataset lacks cross-category branch edits")
			fail++
		} else {
			r, p := skuSet(ref), skuSet(push)
			extra, missing := 0, 0
			for sku := range p {
				if _, ok := r[sku]; !ok {
					extra++
				}
			}
			for sku := range r {
				if _, ok := p[sku]; !ok {
					missing++
				}
			}
			fmt.Printf("  OK: pushdown is wrong by %d spurious + %d missing rows\n", extra, missing)
		}
	}
	return fail
}

func sameRows(a, b []row) bool {
	if len(a) != len(b) {
		return false
	}
	am, bm := skuSet(a), skuSet(b)
	for sku, ra := range am {
		rb, ok := bm[sku]
		if !ok || ra != rb {
			return false
		}
	}
	return true
}

// --- benchmarks ---

type stats struct {
	p50, p95, p99, max time.Duration
	n                  int
}

func measure(f func() error, iter int) stats {
	ds := make([]time.Duration, 0, iter)
	for i := 0; i < iter; i++ {
		t0 := time.Now()
		must(f())
		ds = append(ds, time.Since(t0))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	pick := func(p float64) time.Duration { return ds[int(float64(len(ds)-1)*p)] }
	return stats{p50: pick(0.50), p95: pick(0.95), p99: pick(0.99), max: ds[len(ds)-1], n: iter}
}

func (s stats) String() string {
	return fmt.Sprintf("p50=%-9v p95=%-9v p99=%-9v max=%-9v (n=%d)",
		s.p50.Round(time.Microsecond), s.p95.Round(time.Microsecond),
		s.p99.Round(time.Microsecond), s.max.Round(time.Microsecond), s.n)
}

func bench(ctx context.Context, pool *pgxpool.Pool) {
	rnd := rand.New(rand.NewSource(42))

	fmt.Println("=== S1 point read by primary key")
	fmt.Println("(a PK filter IS safe inside the arms: a row's PK is its identity for")
	fmt.Println(" all of history (§3.2), so it cannot resurface a row the way a value")
	fmt.Println(" predicate can)")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		s := measure(func() error {
			sku := fmt.Sprintf("sku-%08d", rnd.Intn(*flagKeys))
			_, err := query(ctx, pool, resolveSQL(segs, fmt.Sprintf("AND v.sku = '%s'", sku)))
			return err
		}, *flagIter)
		fmt.Printf("  depth %d: %s\n", depth, s)
	}

	// The per-column index is what makes pass 1 of a filtered read an index
	// lookup instead of a full segment scan (DESIGN.md §14.3). Measure both, so
	// the cost of NOT having it is on the record.
	fmt.Println("\n=== S1 filtered scan, two-pass, ~0.1% selectivity, NO per-column index")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		s := measure(func() error {
			cat := fmt.Sprintf("cat-%04d", rnd.Intn(1000))
			_, err := query(ctx, pool, filteredTwoPass(segs, cat))
			return err
		}, 3)
		fmt.Printf("  depth %d: %s\n", depth, s)
	}

	fmt.Println("\n  creating per-column index on (branch_id, category, seq_from, seq_to)...")
	t0 := time.Now()
	_, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS v_products_cat
		ON datagit_v_products (branch_id, category, seq_from, seq_to)`)
	must(err)
	_, err = pool.Exec(ctx, `ANALYZE datagit_v_products`)
	must(err)
	fmt.Printf("  built in %v\n", time.Since(t0).Round(time.Second))

	fmt.Println("\n=== S1 filtered scan, two-pass, ~0.1% selectivity, WITH per-column index")
	fmt.Println("(unbounded: resolves every one of ~10,000 matching keys)")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		s := measure(func() error {
			cat := fmt.Sprintf("cat-%04d", rnd.Intn(1000))
			_, err := query(ctx, pool, filteredTwoPass(segs, cat))
			return err
		}, 10)
		fmt.Printf("  depth %d: %s\n", depth, s)
	}

	fmt.Println("\n=== S1 filtered scan, PAGED (limit 100) — the shape the read API issues")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		s := measure(func() error {
			cat := fmt.Sprintf("cat-%04d", rnd.Intn(1000))
			after := fmt.Sprintf("sku-%08d", rnd.Intn(*flagKeys))
			_, err := query(ctx, pool, filteredTwoPassPaged(segs, cat, after, 100))
			return err
		}, maxi(*flagIter/2, 50))
		fmt.Printf("  depth %d: %s\n", depth, s)
	}

	fmt.Println("\n=== S1 full branch scan (bounded by table size by definition)")
	fmt.Println("(counted server-side: shipping 10M rows to the client would measure")
	fmt.Println(" client marshalling, not resolution)")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		t0 := time.Now()
		var n int64
		must(pool.QueryRow(ctx,
			fmt.Sprintf("SELECT count(*) FROM (%s) z", resolveSQL(segs, ""))).Scan(&n))
		fmt.Printf("  depth %d: %-10v  %d live rows\n", depth,
			time.Since(t0).Round(time.Millisecond), n)
	}
}

// benchFiltered measures only the filtered shapes, assuming the per-column index
// is already built.
func benchFiltered(ctx context.Context, pool *pgxpool.Pool) {
	rnd := rand.New(rand.NewSource(99))
	_, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS v_products_cat
		ON datagit_v_products (branch_id, category, seq_from, seq_to)`)
	must(err)

	fmt.Println("=== S1 filtered scan, unbounded (~10,000 matching rows resolved)")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		s := measure(func() error {
			cat := fmt.Sprintf("cat-%04d", rnd.Intn(1000))
			_, err := query(ctx, pool, filteredTwoPass(segs, cat))
			return err
		}, 10)
		fmt.Printf("  depth %d: %s\n", depth, s)
	}

	fmt.Println("\n=== S1 filtered scan, PAGED (limit 100) — what the read API issues")
	for _, depth := range []int{1, 3, 8} {
		segs := chain(depth)
		s := measure(func() error {
			cat := fmt.Sprintf("cat-%04d", rnd.Intn(1000))
			after := fmt.Sprintf("sku-%08d", rnd.Intn(*flagKeys))
			_, err := query(ctx, pool, filteredTwoPassPaged(segs, cat, after, 100))
			return err
		}, maxi(*flagIter, 100))
		fmt.Printf("  depth %d: %s\n", depth, s)
	}
}

func explain(ctx context.Context, pool *pgxpool.Pool) {
	show := func(label, sql string) {
		fmt.Printf("\n----- %s\n", label)
		rs, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, SUMMARY) "+sql)
		must(err)
		defer rs.Close()
		for rs.Next() {
			var line string
			must(rs.Scan(&line))
			fmt.Println("  " + line)
		}
	}
	show("point read, depth 3", resolveSQL(chain(3), "AND v.sku = 'sku-04242424'"))
	show("point read, depth 8", resolveSQL(chain(8), "AND v.sku = 'sku-04242424'"))
	show("filtered two-pass, depth 3", filteredTwoPass(chain(3), "cat-0042"))
	show("filtered two-pass, depth 8", filteredTwoPass(chain(8), "cat-0042"))
	show("filtered naive (resolve then filter), depth 3", filteredNaive(chain(3), "cat-0042"))
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *flagDSN)
	must(err)
	defer pool.Close()

	switch *flagMode {
	case "correctness":
		if n := correctness(ctx, pool); n > 0 {
			fmt.Printf("\n%d correctness checks FAILED\n", n)
			os.Exit(1)
		}
		fmt.Println("\nall correctness checks passed")
	case "bench":
		bench(ctx, pool)
	case "filtered":
		// Assumes the per-column index already exists; measures only the
		// filtered shapes.
		benchFiltered(ctx, pool)
	case "explain":
		explain(ctx, pool)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode")
		os.Exit(2)
	}
}
