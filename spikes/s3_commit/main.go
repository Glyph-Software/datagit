// S3: atomic commit latency and write amplification (PLAN.md Phase 0).
// THROWAWAY spike code — not part of the shipped tree.
//
// Measures the DESIGN.md §6.1 write path: one RPC, one transaction, ref lock ->
// expected_head check -> PK-ordered SELECT FOR UPDATE -> live writes -> close and
// insert sidecar versions -> commit record -> advance ref.
//
//	go run ./spikes/s3_commit -mode setup
//	go run ./spikes/s3_commit -mode latency
//	go run ./spikes/s3_commit -mode concurrency
//	go run ./spikes/s3_commit -mode amplification
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	flagDSN  = flag.String("dsn", "postgres://datagit:datagit@localhost:55417/datagit", "postgres DSN")
	flagMode = flag.String("mode", "latency", "setup | latency | concurrency | amplification")
	flagRows = flag.Int("rows", 1000000, "live rows to seed")
	flagIter = flag.Int("iter", 300, "iterations per measurement")
	flagDur  = flag.Duration("dur", 10*time.Second, "duration per concurrency step")
)

const schema = `
DROP SCHEMA IF EXISTS s3 CASCADE;
CREATE SCHEMA s3;

-- The application's own table. Schema UNMODIFIED: no version columns, no
-- triggers. DESIGN.md §5.1 — it must stay a clean materialization of main@HEAD.
CREATE TABLE s3.products (
    sku        text PRIMARY KEY,
    name       text,
    category   text,
    price      numeric(12,2),
    updated_at timestamptz
);

CREATE TABLE s3.datagit_v_products (
    version_id   bigserial PRIMARY KEY,
    branch_id    uuid   NOT NULL,
    seq_from     bigint NOT NULL,
    seq_to       bigint NOT NULL DEFAULT 9223372036854775807,
    op           smallint NOT NULL,
    commit_id    bytea  NOT NULL,
    session_id   uuid,
    changed_cols bytea  NOT NULL,
    sku          text   NOT NULL,
    name         text,
    category     text,
    price        numeric(12,2),
    updated_at   timestamptz
);
CREATE INDEX v_products_resolve ON s3.datagit_v_products (branch_id, sku, seq_from DESC);
CREATE INDEX v_products_range   ON s3.datagit_v_products (branch_id, seq_from, seq_to);
CREATE INDEX v_products_commit  ON s3.datagit_v_products (commit_id);

CREATE TABLE s3.datagit_commit (
    id            bytea PRIMARY KEY,
    branch_id     uuid   NOT NULL,
    seq           bigint NOT NULL,
    parent_ids    bytea[] NOT NULL,
    author        text   NOT NULL,
    committed_at  timestamptz NOT NULL DEFAULT now(),
    message       text   NOT NULL,
    change_digest bytea  NOT NULL
);

CREATE TABLE s3.datagit_ref (
    id          uuid PRIMARY KEY,
    name        text NOT NULL,
    head_commit bytea,
    head_seq    bigint NOT NULL DEFAULT 0
);
`

const mainBranch = "00000000-0000-0000-0000-000000000000"

func setup(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Printf("=== setup: %d live rows\n", *flagRows)
	t0 := time.Now()
	must(exec(ctx, pool, schema))
	must(exec(ctx, pool, `
		INSERT INTO s3.products (sku, name, category, price, updated_at)
		SELECT 'sku-' || lpad(k::text, 8, '0'), 'product ' || k,
		       'cat-' || lpad((k % 1000)::text, 4, '0'),
		       ((k % 90000) + 1000)::numeric / 100,
		       timestamptz '2020-01-01' + (k % 2000) * interval '1 day'
		FROM generate_series(0, $1 - 1) k`, *flagRows))
	// The sidecar carries an open version per live row (§5.2c duplication).
	must(exec(ctx, pool, `
		INSERT INTO s3.datagit_v_products
		  (branch_id, seq_from, seq_to, op, commit_id, changed_cols,
		   sku, name, category, price, updated_at)
		SELECT $1::uuid, 0, 9223372036854775807, 1,
		       int8send(0::bigint)||int8send(0::bigint)||int8send(0::bigint)||int8send(0::bigint),
		       '\x0f'::bytea, sku, name, category, price, updated_at
		FROM s3.products`, mainBranch))
	must(exec(ctx, pool, `INSERT INTO s3.datagit_ref (id, name, head_seq) VALUES ($1::uuid, 'main', 0)`, mainBranch))
	must(exec(ctx, pool, `ANALYZE s3.products; ANALYZE s3.datagit_v_products;`))
	fmt.Printf("    done in %v\n", time.Since(t0).Round(time.Millisecond))
}

// baselineCommit is what the application would do today: K plain UPDATEs in one
// transaction, no history at all.
func baselineCommit(ctx context.Context, pool *pgxpool.Pool, skus []string, price float64) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, sku := range skus {
			if _, err := tx.Exec(ctx,
				`UPDATE s3.products SET price = $1, updated_at = now() WHERE sku = $2`,
				price, sku); err != nil {
				return err
			}
		}
		return nil
	})
}

// datagitCommit is the DESIGN.md §6.1 write path, batched per §14.3.
//
// Statement count is constant in the change-set size: the per-row work is done
// with array-valued statements rather than a round trip per row, which is what
// the SDK's buffered Commit makes possible.
func datagitCommit(ctx context.Context, pool *pgxpool.Pool, skus []string, price float64) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// 1. Ref lock. Serializes seq assignment for this branch (§11.3).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, mainBranch); err != nil {
			return err
		}
		// 2. Read the head (the expected_head check would compare here).
		var seq int64
		if err := tx.QueryRow(ctx,
			`SELECT head_seq FROM s3.datagit_ref WHERE id = $1::uuid FOR UPDATE`,
			mainBranch).Scan(&seq); err != nil {
			return err
		}
		newSeq := seq + 1

		// 3. Lock the target rows in primary-key order (§6.1 property 3).
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM s3.products WHERE sku = ANY($1) ORDER BY sku FOR UPDATE`,
			skus); err != nil {
			return err
		}
		// 4. Live-table write.
		if _, err := tx.Exec(ctx,
			`UPDATE s3.products SET price = $1, updated_at = now() WHERE sku = ANY($2)`,
			price, skus); err != nil {
			return err
		}
		// 5. Close the superseded open versions.
		if _, err := tx.Exec(ctx,
			`UPDATE s3.datagit_v_products SET seq_to = $1
			  WHERE branch_id = $2::uuid AND sku = ANY($3)
			    AND session_id IS NULL AND seq_to = 9223372036854775807`,
			newSeq, mainBranch, skus); err != nil {
			return err
		}
		// 6. Insert the new open versions, already stamped with the commit id.
		commitID := make([]byte, 32)
		for i := range commitID {
			commitID[i] = byte(newSeq >> (i % 8))
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO s3.datagit_v_products
			   (branch_id, seq_from, seq_to, op, commit_id, changed_cols,
			    sku, name, category, price, updated_at)
			 SELECT $1::uuid, $2, 9223372036854775807, 2, $3, '\x08'::bytea,
			        p.sku, p.name, p.category, p.price, p.updated_at
			   FROM s3.products p WHERE p.sku = ANY($4)`,
			mainBranch, newSeq, commitID, skus); err != nil {
			return err
		}
		// 7. Commit record and ref advance.
		if _, err := tx.Exec(ctx,
			`INSERT INTO s3.datagit_commit
			   (id, branch_id, seq, parent_ids, author, message, change_digest)
			 VALUES ($1, $2::uuid, $3, ARRAY[]::bytea[], 'bench', 'spike', $1)`,
			commitID, mainBranch, newSeq); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE s3.datagit_ref SET head_commit = $1, head_seq = $2 WHERE id = $3::uuid`,
			commitID, newSeq, mainBranch)
		return err
	})
}

type stats struct{ p50, p95, p99, max time.Duration }

func summarize(ds []time.Duration) stats {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	pick := func(p float64) time.Duration { return ds[int(float64(len(ds)-1)*p)] }
	return stats{pick(0.50), pick(0.95), pick(0.99), ds[len(ds)-1]}
}

func (s stats) String() string {
	return fmt.Sprintf("p50=%-8v p95=%-8v p99=%-8v max=%-8v",
		s.p50.Round(time.Microsecond), s.p95.Round(time.Microsecond),
		s.p99.Round(time.Microsecond), s.max.Round(time.Microsecond))
}

func randomSkus(rnd *rand.Rand, n int) []string {
	seen := map[int]bool{}
	out := make([]string, 0, n)
	for len(out) < n {
		k := rnd.Intn(*flagRows)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, fmt.Sprintf("sku-%08d", k))
	}
	sort.Strings(out) // PK order, per §6.1
	return out
}

func latency(ctx context.Context, pool *pgxpool.Pool) {
	rnd := rand.New(rand.NewSource(7))
	fmt.Println("=== S3 single-writer commit latency by change-set size")
	fmt.Printf("%-8s  %-52s  %-52s  %s\n", "size", "baseline (plain UPDATEs, no history)", "datagit (one transaction, with history)", "overhead p99")
	for _, size := range []int{1, 10, 100, 1000} {
		iter := *flagIter
		if size >= 100 {
			iter = maxi(iter/10, 30)
		}
		bs := make([]time.Duration, 0, iter)
		ds := make([]time.Duration, 0, iter)
		for i := 0; i < iter; i++ {
			skus := randomSkus(rnd, size)
			t0 := time.Now()
			must(baselineCommit(ctx, pool, skus, float64(100+i)))
			bs = append(bs, time.Since(t0))

			skus = randomSkus(rnd, size)
			t0 = time.Now()
			must(datagitCommit(ctx, pool, skus, float64(200+i)))
			ds = append(ds, time.Since(t0))
		}
		b, d := summarize(bs), summarize(ds)
		fmt.Printf("%-8d  %-52s  %-52s  %v\n", size, b, d,
			(d.p99 - b.p99).Round(time.Microsecond))
	}
}

func concurrency(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("=== S3 concurrent committers (change-set size 1, disjoint keys)")
	fmt.Println("Every commit to a branch takes the same ref advisory lock, so commits")
	fmt.Println("to one branch are fully serialized by design (§11.3). This measures")
	fmt.Println("what that costs.")
	fmt.Printf("\n%-10s  %-12s  %-52s\n", "writers", "commits/s", "latency")
	for _, writers := range []int{1, 10, 50, 100} {
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			all     []time.Duration
			count   atomic.Int64
			stop    = time.Now().Add(*flagDur)
			startWG sync.WaitGroup
		)
		startWG.Add(1)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				rnd := rand.New(rand.NewSource(int64(seed)))
				var local []time.Duration
				startWG.Wait()
				for time.Now().Before(stop) {
					skus := randomSkus(rnd, 1)
					t0 := time.Now()
					if err := datagitCommit(ctx, pool, skus, float64(seed)); err != nil {
						continue
					}
					local = append(local, time.Since(t0))
					count.Add(1)
				}
				mu.Lock()
				all = append(all, local...)
				mu.Unlock()
			}(w)
		}
		t0 := time.Now()
		startWG.Done()
		wg.Wait()
		elapsed := time.Since(t0)
		if len(all) == 0 {
			fmt.Printf("%-10d  %-12s  no successful commits\n", writers, "-")
			continue
		}
		fmt.Printf("%-10d  %-12.0f  %-52s\n", writers,
			float64(count.Load())/elapsed.Seconds(), summarize(all))
	}
}

// amplification measures WAL bytes generated, which is the honest measure of
// write amplification: it counts everything the database must durably record,
// including index maintenance.
func amplification(ctx context.Context, pool *pgxpool.Pool) {
	rnd := rand.New(rand.NewSource(11))
	fmt.Println("=== S3 write amplification (WAL bytes per changed row)")
	fmt.Printf("%-8s  %-16s  %-16s  %s\n", "size", "baseline B/row", "datagit B/row", "amplification")

	walBytes := func(f func()) int64 {
		var before, after string
		must(pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&before))
		f()
		must(pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&after))
		var diff int64
		must(pool.QueryRow(ctx, `SELECT ($1::pg_lsn - $2::pg_lsn)::bigint`, after, before).Scan(&diff))
		return diff
	}

	for _, size := range []int{1, 10, 100} {
		const reps = 30
		b := walBytes(func() {
			for i := 0; i < reps; i++ {
				must(baselineCommit(ctx, pool, randomSkus(rnd, size), float64(300+i)))
			}
		})
		d := walBytes(func() {
			for i := 0; i < reps; i++ {
				must(datagitCommit(ctx, pool, randomSkus(rnd, size), float64(400+i)))
			}
		})
		bp := float64(b) / float64(reps*size)
		dp := float64(d) / float64(reps*size)
		fmt.Printf("%-8d  %-16.0f  %-16.0f  %.2fx\n", size, bp, dp, dp/bp)
	}
}

func exec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
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
	cfg, err := pgxpool.ParseConfig(*flagDSN)
	must(err)
	cfg.MaxConns = 120
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer pool.Close()

	switch *flagMode {
	case "setup":
		setup(ctx, pool)
	case "latency":
		latency(ctx, pool)
	case "concurrency":
		concurrency(ctx, pool)
	case "amplification":
		amplification(ctx, pool)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode")
		os.Exit(2)
	}
}
