package main

// S3 retry: write-path variants for the amplification failure.
//
// Phase 0 measured 8.7-9.4x amplification against a <=3x bar. The levers named
// in docs/phase0/findings.md were guesses; this measures them.
//
// The variants, in increasing order of how much they change the design:
//
//	full      the design as written: 4 sidecar indexes, close-then-insert
//	nocommit  drop the commit_id index (findings.md's first suggested lever)
//	noseqto   keep seq_to but drop it from the range index
//	append    NO seq_to column at all: a version is valid until the next
//	          version of the same key. Eliminates the close UPDATE entirely.
//
// `append` is the interesting one. DESIGN.md §5.2 stores an explicit half-open
// [seq_from, seq_to) interval, which means every write must UPDATE the previous
// open version to close it. That UPDATE is non-HOT — seq_to sits in the range
// index — so it rewrites every index entry for the row. Deriving the upper bound
// from the next version's seq_from removes the write and the column.

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type variant struct {
	name    string
	desc    string
	seqTo   bool     // store an explicit seq_to and close the previous version
	indexes []string // index definitions, %s is the table name
}

var variants = []variant{
	{
		name:  "full",
		desc:  "as designed: explicit seq_to, 4 indexes",
		seqTo: true,
		indexes: []string{
			`CREATE INDEX vx_%[2]s_resolve ON %[1]s (branch_id, sku, seq_from DESC)`,
			`CREATE INDEX vx_%[2]s_range   ON %[1]s (branch_id, seq_from, seq_to)`,
			`CREATE INDEX vx_%[2]s_commit  ON %[1]s (commit_id)`,
			`CREATE INDEX vx_%[2]s_session ON %[1]s (session_id) WHERE session_id IS NOT NULL`,
		},
	},
	{
		name:  "nocommit",
		desc:  "drop the commit_id index",
		seqTo: true,
		indexes: []string{
			`CREATE INDEX vx_%[2]s_resolve ON %[1]s (branch_id, sku, seq_from DESC)`,
			`CREATE INDEX vx_%[2]s_range   ON %[1]s (branch_id, seq_from, seq_to)`,
			`CREATE INDEX vx_%[2]s_session ON %[1]s (session_id) WHERE session_id IS NOT NULL`,
		},
	},
	{
		name:  "noseqto",
		desc:  "seq_to kept but removed from the range index",
		seqTo: true,
		indexes: []string{
			`CREATE INDEX vx_%[2]s_resolve ON %[1]s (branch_id, sku, seq_from DESC)`,
			`CREATE INDEX vx_%[2]s_range   ON %[1]s (branch_id, seq_from)`,
			`CREATE INDEX vx_%[2]s_session ON %[1]s (session_id) WHERE session_id IS NOT NULL`,
		},
	},
	{
		name:  "append",
		desc:  "no seq_to column: validity ends at the next version of the key",
		seqTo: false,
		indexes: []string{
			`CREATE INDEX vx_%[2]s_resolve ON %[1]s (branch_id, sku, seq_from DESC)`,
			`CREATE INDEX vx_%[2]s_range   ON %[1]s (branch_id, seq_from)`,
			`CREATE INDEX vx_%[2]s_session ON %[1]s (session_id) WHERE session_id IS NOT NULL`,
		},
	},
}

func (v variant) ddl(tbl string) string {
	seqToCol := ""
	if v.seqTo {
		seqToCol = "seq_to bigint NOT NULL DEFAULT 9223372036854775807,"
	}
	return fmt.Sprintf(`
		CREATE TABLE %s (
			branch_id    uuid   NOT NULL,
			seq_from     bigint NOT NULL,
			%s
			op           smallint NOT NULL,
			commit_id    bytea  NOT NULL,
			session_id   uuid,
			changed_cols bytea  NOT NULL,
			sku          text   NOT NULL,
			name         text, category text,
			price        numeric(12,2), updated_at timestamptz,
			PRIMARY KEY (branch_id, sku, seq_from)
		)`, tbl, seqToCol)
}

// commit runs one commit through this variant's write path.
func (v variant) commit(ctx context.Context, pool *pgxpool.Pool, tbl string, skus []string, price float64, seq int64) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, mainBranch); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM s3v.products WHERE sku = ANY($1) ORDER BY sku FOR UPDATE`, skus); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE s3v.products SET price = $1, updated_at = now() WHERE sku = ANY($2)`,
			price, skus); err != nil {
			return err
		}

		// THE DIFFERENCE. With an explicit seq_to, every write must first close
		// the previous open version. That UPDATE is non-HOT whenever seq_to is
		// indexed, so it rewrites every index entry for the row.
		if v.seqTo {
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				`UPDATE %s SET seq_to = $1
				  WHERE branch_id = $2::uuid AND sku = ANY($3)
				    AND session_id IS NULL AND seq_to = 9223372036854775807`, tbl),
				seq, mainBranch, skus); err != nil {
				return err
			}
		}

		commitID := make([]byte, 32)
		for i := range commitID {
			commitID[i] = byte(seq >> (i % 8))
		}
		cols, vals := "branch_id, seq_from, op, commit_id, changed_cols, sku, name, category, price, updated_at",
			"$1::uuid, $2, 2, $3, '\\x08'::bytea, p.sku, p.name, p.category, p.price, p.updated_at"
		if v.seqTo {
			cols = "branch_id, seq_from, seq_to, op, commit_id, changed_cols, sku, name, category, price, updated_at"
			vals = "$1::uuid, $2, 9223372036854775807, 2, $3, '\\x08'::bytea, p.sku, p.name, p.category, p.price, p.updated_at"
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (%s) SELECT %s FROM s3v.products p WHERE p.sku = ANY($4)`,
			tbl, cols, vals), mainBranch, seq, commitID, skus); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO s3v.datagit_commit (id, branch_id, seq, parent_ids, author, message, change_digest)
			 VALUES ($1, $2::uuid, $3, ARRAY[]::bytea[], 'bench', 'spike', $1)`,
			commitID, mainBranch, seq); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE s3v.datagit_ref SET head_commit = $1, head_seq = $2 WHERE id = $3::uuid`,
			commitID, seq, mainBranch)
		return err
	})
}

func variantSetup(ctx context.Context, pool *pgxpool.Pool, rows int) {
	must(exec(ctx, pool, `DROP SCHEMA IF EXISTS s3v CASCADE; CREATE SCHEMA s3v;`))
	must(exec(ctx, pool, `
		CREATE TABLE s3v.products (
			sku text PRIMARY KEY, name text, category text,
			price numeric(12,2), updated_at timestamptz)`))
	must(exec(ctx, pool, `
		INSERT INTO s3v.products
		SELECT 'sku-'||lpad(k::text,8,'0'), 'product '||k,
		       'cat-'||lpad((k%1000)::text,4,'0'),
		       ((k%90000)+1000)::numeric/100, timestamptz '2020-01-01'
		FROM generate_series(0,$1-1) k`, rows))
	must(exec(ctx, pool, `
		CREATE TABLE s3v.datagit_commit (
			id bytea PRIMARY KEY, branch_id uuid NOT NULL, seq bigint NOT NULL,
			parent_ids bytea[] NOT NULL, author text NOT NULL,
			committed_at timestamptz NOT NULL DEFAULT now(),
			message text NOT NULL, change_digest bytea NOT NULL)`))
	must(exec(ctx, pool, `
		CREATE TABLE s3v.datagit_ref (
			id uuid PRIMARY KEY, name text NOT NULL,
			head_commit bytea, head_seq bigint NOT NULL DEFAULT 0)`))
	must(exec(ctx, pool, `INSERT INTO s3v.datagit_ref (id, name) VALUES ($1::uuid, 'main')`, mainBranch))

	for _, v := range variants {
		tbl := "s3v.v_" + v.name
		must(exec(ctx, pool, v.ddl(tbl)))
		seqToVal := ""
		if v.seqTo {
			seqToVal = "9223372036854775807, "
		}
		seqToCol := ""
		if v.seqTo {
			seqToCol = "seq_to, "
		}
		must(exec(ctx, pool, fmt.Sprintf(`
			INSERT INTO %s (branch_id, seq_from, %sop, commit_id, changed_cols,
			                sku, name, category, price, updated_at)
			SELECT $1::uuid, 0, %s1,
			       int8send(0::bigint)||int8send(0::bigint)||int8send(0::bigint)||int8send(0::bigint),
			       '\x0f'::bytea, sku, name, category, price, updated_at
			FROM s3v.products`, tbl, seqToCol, seqToVal), mainBranch))
		for _, idx := range v.indexes {
			must(exec(ctx, pool, fmt.Sprintf(idx, tbl, v.name)))
		}
		must(exec(ctx, pool, fmt.Sprintf(`ANALYZE %s`, tbl)))
	}
	must(exec(ctx, pool, `ANALYZE s3v.products`))
}

// variantAmplification is the S3 retry.
func variantAmplification(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Printf("=== S3 RETRY: write amplification by write-path variant (%d live rows)\n", *flagRows)
	variantSetup(ctx, pool, *flagRows)

	walBytes := func(f func()) int64 {
		var before, after string
		must(pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&before))
		f()
		must(pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&after))
		var diff int64
		must(pool.QueryRow(ctx, `SELECT ($1::pg_lsn - $2::pg_lsn)::bigint`, after, before).Scan(&diff))
		return diff
	}

	const reps = 40
	var seq int64 // monotonic across the whole run: commit ids must stay unique

	// Each measurement is preceded by an identical warmup pass.
	//
	// Without it the numbers are dominated by full-page writes: after a
	// checkpoint, the first write to a page logs the entire 8 KB page, and the
	// variants touch more distinct pages than the baseline does, so the
	// comparison would measure page-touching rather than logical write volume.
	// Warming leaves the pages dirty and measures steady state.
	for _, size := range []int{1, 10, 100} {
		rnd := rand.New(rand.NewSource(int64(size)))
		fmt.Printf("\nchange-set size %d\n", size)

		// Baseline: plain UPDATEs, no history at all.
		baseRun := func(tag float64) {
			for i := 0; i < reps; i++ {
				must(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx,
						`UPDATE s3v.products SET price = $1, updated_at = now() WHERE sku = ANY($2)`,
						tag+float64(i), randomSkusN(rnd, size))
					return err
				}))
			}
		}
		baseRun(100) // warmup
		base := walBytes(func() { baseRun(150) })
		bp := float64(base) / float64(reps*size)
		fmt.Printf("  %-10s %-52s %8.0f B/row   1.00x\n", "baseline", "plain UPDATEs, no history", bp)

		for _, v := range variants {
			tbl := "s3v.v_" + v.name
			run := func(tag float64) {
				for i := 0; i < reps; i++ {
					seq++
					must(v.commit(ctx, pool, tbl, randomSkusN(rnd, size), tag+float64(i), seq))
				}
			}
			run(200) // warmup
			got := walBytes(func() { run(250) })
			vp := float64(got) / float64(reps*size)
			fmt.Printf("  %-10s %-52s %8.0f B/row  %5.2fx\n", v.name, v.desc, vp, vp/bp)
		}
	}

	fmt.Println("\n=== index and table sizes after the run")
	for _, v := range variants {
		tbl := "s3v.v_" + v.name
		var heap, idx int64
		must(pool.QueryRow(ctx, `SELECT pg_table_size($1), pg_indexes_size($1)`, tbl).Scan(&heap, &idx))
		fmt.Printf("  %-10s heap %6.1f MB   indexes %6.1f MB\n",
			v.name, float64(heap)/1024/1024, float64(idx)/1024/1024)
	}
}

// variantResolveCheck confirms the `append` variant can still answer the two
// queries the interval model was chosen for, and how fast.
func variantResolveCheck(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("\n=== can the append-only variant still resolve?")
	fmt.Println("With no seq_to, a version is valid until the next version of the same")
	fmt.Println("key, so `state at seq c` is the greatest seq_from <= c per key.")

	var headSeq int64
	must(pool.QueryRow(ctx, `SELECT head_seq FROM s3v.datagit_ref WHERE id = $1::uuid`, mainBranch).Scan(&headSeq))

	type q struct{ name, sql string }
	queries := []q{
		{"interval model, point read", `
			SELECT sku, price FROM s3v.v_full
			 WHERE branch_id = $1::uuid AND sku = $2
			   AND seq_from <= $3 AND seq_to > $3 AND op <> 3`},
		{"append model, point read", `
			SELECT sku, price FROM (
				SELECT DISTINCT ON (sku) sku, price, op FROM s3v.v_append
				 WHERE branch_id = $1::uuid AND sku = $2 AND seq_from <= $3
				 ORDER BY sku, seq_from DESC
			) r WHERE r.op <> 3`},
	}
	// The real question for the append model: interval predicates make "all live
	// rows at seq c" a plain range scan, while append needs a top-1-per-key
	// aggregate. Measure that, not just point reads.
	fullScans := []q{
		{"interval model, full branch scan", `
			SELECT count(*) FROM s3v.v_full
			 WHERE branch_id = $1::uuid AND seq_from <= $2 AND seq_to > $2 AND op <> 3`},
		{"append model, full branch scan", `
			SELECT count(*) FROM (
				SELECT DISTINCT ON (sku) sku, op FROM s3v.v_append
				 WHERE branch_id = $1::uuid AND seq_from <= $2
				 ORDER BY sku, seq_from DESC
			) r WHERE r.op <> 3`},
	}
	for _, qq := range fullScans {
		var n int64
		t0 := time.Now()
		must(pool.QueryRow(ctx, qq.sql, mainBranch, headSeq).Scan(&n))
		d1 := time.Since(t0)
		t0 = time.Now()
		must(pool.QueryRow(ctx, qq.sql, mainBranch, headSeq).Scan(&n))
		fmt.Printf("  %-34s cold %-9v warm %-9v (%d rows)\n",
			qq.name, d1.Round(time.Millisecond), time.Since(t0).Round(time.Millisecond), n)
	}

	rnd := rand.New(rand.NewSource(5))
	for _, qq := range queries {
		ds := make([]time.Duration, 0, 300)
		for i := 0; i < 300; i++ {
			sku := fmt.Sprintf("sku-%08d", rnd.Intn(*flagRows))
			t0 := time.Now()
			rows, err := pool.Query(ctx, qq.sql, mainBranch, sku, headSeq)
			must(err)
			for rows.Next() {
			}
			rows.Close()
			ds = append(ds, time.Since(t0))
		}
		fmt.Printf("  %-30s %s\n", qq.name, summarize(ds))
	}
}

func randomSkusN(rnd *rand.Rand, n int) []string {
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
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
