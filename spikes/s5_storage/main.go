// S5: storage growth and pruning (PLAN.md Phase 0).
// THROWAWAY spike code — not part of the shipped tree.
//
// Answers two questions:
//   1. Is the DESIGN.md §5.2c / §14.2 estimate real — ~2x the data plus indexes,
//      3-4x at rest before any history?
//   2. Does partition-drop pruning beat row deletion by an order of magnitude
//      (§14.3)?
//
//	go run ./spikes/s5_storage -mode sizes
//	go run ./spikes/s5_storage -mode pruning
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	flagDSN     = flag.String("dsn", "postgres://datagit:datagit@localhost:55417/datagit", "postgres DSN")
	flagMode    = flag.String("mode", "sizes", "sizes | pruning")
	flagRows    = flag.Int("rows", 2000000, "live rows")
	flagChurn   = flag.Int("churn", 4, "historical versions per row")
	flagParts   = flag.Int("parts", 8, "partitions for the pruning test")
	flagDropPct = flag.Int("droppct", 25, "percent of data to prune")
)

func sizes(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Printf("=== S5 storage at rest: %d live rows, %d historical versions each\n\n", *flagRows, *flagChurn)
	must(exec(ctx, pool, `DROP SCHEMA IF EXISTS s5 CASCADE; CREATE SCHEMA s5;`))

	// The application's own table, schema unmodified (§5.1).
	must(exec(ctx, pool, `
		CREATE TABLE s5.products (
			sku text PRIMARY KEY, name text, category text,
			price numeric(12,2), updated_at timestamptz);`))
	must(exec(ctx, pool, `
		INSERT INTO s5.products
		SELECT 'sku-'||lpad(k::text,8,'0'), 'product '||k,
		       'cat-'||lpad((k%1000)::text,4,'0'),
		       ((k%90000)+1000)::numeric/100,
		       timestamptz '2020-01-01' + (k%2000)*interval '1 day'
		FROM generate_series(0,$1-1) k`, *flagRows))

	sidecarDDL := func(name string) string {
		return fmt.Sprintf(`
		CREATE TABLE %s (
			version_id bigserial PRIMARY KEY,
			branch_id uuid NOT NULL,
			seq_from bigint NOT NULL,
			seq_to bigint NOT NULL DEFAULT 9223372036854775807,
			op smallint NOT NULL,
			commit_id bytea NOT NULL,
			session_id uuid,
			changed_cols bytea NOT NULL,
			sku text NOT NULL, name text, category text,
			price numeric(12,2), updated_at timestamptz);`, name)
	}
	// fill writes `closed` closed versions per row, plus one open version if
	// openIdx >= 0.
	//
	// The tiers differ exactly as §5.2c and §3.4 describe: `versioned` carries an
	// open version duplicating every live row (so historical reads are a pure
	// sidecar query), while `audit` stores closed intervals only, because it
	// never needs branch resolution.
	fill := func(name string, closed int, withOpen bool) {
		last := -1
		total := closed - 1
		if withOpen {
			last = closed // one extra, open
			total = closed
		}
		must(exec(ctx, pool, fmt.Sprintf(`
			INSERT INTO %s (branch_id, seq_from, seq_to, op, commit_id, changed_cols,
			                sku, name, category, price, updated_at)
			SELECT '00000000-0000-0000-0000-000000000000'::uuid, v.i,
			       CASE WHEN v.i = $2 THEN 9223372036854775807 ELSE v.i + 1 END,
			       2, int8send(v.i::bigint)||int8send(0::bigint)||int8send(0::bigint)||int8send(0::bigint),
			       '\x08'::bytea, p.sku, p.name, p.category, p.price, p.updated_at
			FROM s5.products p CROSS JOIN generate_series(0,$1) AS v(i)`, name),
			total, last))
	}
	idx := func(name string, full bool) {
		short := name[len(name)-3:]
		must(exec(ctx, pool, fmt.Sprintf(
			`CREATE INDEX %s_range ON %s (branch_id, seq_from, seq_to)`, short, name)))
		must(exec(ctx, pool, fmt.Sprintf(
			`CREATE INDEX %s_commit ON %s (commit_id)`, short, name)))
		if full {
			// versioned tier also needs the per-key resolve index and the
			// partial session index (§5.2).
			must(exec(ctx, pool, fmt.Sprintf(
				`CREATE INDEX %s_resolve ON %s (branch_id, sku, seq_from DESC)`, short, name)))
			must(exec(ctx, pool, fmt.Sprintf(
				`CREATE INDEX %s_session ON %s (session_id) WHERE session_id IS NOT NULL`, short, name)))
		}
	}

	// versioned tier: an open version per live row PLUS history (§5.2c).
	must(exec(ctx, pool, sidecarDDL("s5.v_versioned")))
	fill("s5.v_versioned", *flagChurn, true)
	idx("s5.v_versioned", true)

	// audit tier: closed intervals only, no open-version duplication, and no
	// need for the resolve or session indexes since it never branches (§3.4).
	must(exec(ctx, pool, sidecarDDL("s5.v_audit")))
	fill("s5.v_audit", *flagChurn-1, false)
	idx("s5.v_audit", false)

	must(exec(ctx, pool, `ANALYZE s5.products; ANALYZE s5.v_versioned; ANALYZE s5.v_audit;`))

	type sz struct{ heap, idx, total int64 }
	get := func(rel string) sz {
		var s sz
		must(pool.QueryRow(ctx, `SELECT pg_table_size($1), pg_indexes_size($1), pg_total_relation_size($1)`,
			rel).Scan(&s.heap, &s.idx, &s.total))
		return s
	}
	base := get("s5.products")
	ver := get("s5.v_versioned")
	aud := get("s5.v_audit")

	mb := func(b int64) string { return fmt.Sprintf("%.0f MB", float64(b)/1024/1024) }
	fmt.Printf("%-24s %-10s %-10s %-10s\n", "relation", "heap", "indexes", "total")
	fmt.Printf("%-24s %-10s %-10s %-10s\n", "products (live table)", mb(base.heap), mb(base.idx), mb(base.total))
	fmt.Printf("%-24s %-10s %-10s %-10s\n", "sidecar, versioned", mb(ver.heap), mb(ver.idx), mb(ver.total))
	fmt.Printf("%-24s %-10s %-10s %-10s\n", "sidecar, audit", mb(aud.heap), mb(aud.idx), mb(aud.total))

	// "At rest" = live table + the open versions only, with history excluded.
	// Prorated by row count, since every sidecar row is the same shape.
	var openRows, allRows int64
	must(pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE seq_to = 9223372036854775807), count(*)
		   FROM s5.v_versioned`).Scan(&openRows, &allRows))
	openTotal := int64(float64(ver.total) * float64(openRows) / float64(allRows))

	fmt.Printf("\nsidecar rows: %d total, %d open (%d closed history)\n",
		allRows, openRows, allRows-openRows)
	fmt.Printf("\nat rest, EXCLUDING history (live table + open versions + their indexes):\n")
	fmt.Printf("  %.2fx the live table\n", float64(base.total+openTotal)/float64(base.total))
	fmt.Printf("\nincluding %d versions of history per row:\n", *flagChurn)
	fmt.Printf("  versioned tier: %.2fx    audit tier: %.2fx\n",
		float64(base.total+ver.total)/float64(base.total),
		float64(base.total+aud.total)/float64(base.total))
}

func pruning(ctx context.Context, pool *pgxpool.Pool) {
	rowsPerPart := *flagRows / *flagParts
	fmt.Printf("=== S5 pruning: %d rows across %d partitions, dropping ~%d%%\n\n",
		*flagRows, *flagParts, *flagDropPct)
	// Parallel workers need more shared memory than Docker's default 64 MB
	// /dev/shm provides. Not a design issue — an environment one — but it would
	// bite any operator running Postgres in a container with the default.
	must(exec(ctx, pool, `SET max_parallel_workers_per_gather = 0`))
	must(exec(ctx, pool, `DROP SCHEMA IF EXISTS s5p CASCADE; CREATE SCHEMA s5p;`))

	// NOTE (finding): PostgreSQL requires every unique constraint on a
	// partitioned table to include the partition key, so the sidecar's surrogate
	// primary key becomes (version_id, seq_from) once it is partitioned.
	// DESIGN.md §5.2 shows a bare `version_id` PK; §14.3 asks for partitioning.
	// Those two cannot both hold.
	partitioned := `
		CREATE TABLE s5p.v_part (
			version_id bigserial, branch_id uuid NOT NULL,
			seq_from bigint NOT NULL, seq_to bigint NOT NULL,
			op smallint NOT NULL, commit_id bytea NOT NULL,
			changed_cols bytea NOT NULL, sku text NOT NULL,
			name text, category text, price numeric(12,2), updated_at timestamptz,
			PRIMARY KEY (version_id, seq_from)
		) PARTITION BY RANGE (seq_from);`
	must(exec(ctx, pool, partitioned))
	for i := 0; i < *flagParts; i++ {
		must(exec(ctx, pool, fmt.Sprintf(
			`CREATE TABLE s5p.v_part_%d PARTITION OF s5p.v_part FOR VALUES FROM (%d) TO (%d)`,
			i, i*rowsPerPart, (i+1)*rowsPerPart)))
	}
	// Unpartitioned twin, same data, for the DELETE comparison.
	must(exec(ctx, pool, `
		CREATE TABLE s5p.v_flat (
			version_id bigserial PRIMARY KEY, branch_id uuid NOT NULL,
			seq_from bigint NOT NULL, seq_to bigint NOT NULL,
			op smallint NOT NULL, commit_id bytea NOT NULL,
			changed_cols bytea NOT NULL, sku text NOT NULL,
			name text, category text, price numeric(12,2), updated_at timestamptz);`))

	insert := func(tbl string) {
		must(exec(ctx, pool, fmt.Sprintf(`
			INSERT INTO %s (branch_id, seq_from, seq_to, op, commit_id, changed_cols,
			                sku, name, category, price, updated_at)
			SELECT '00000000-0000-0000-0000-000000000000'::uuid, k, k+1, 2,
			       int8send(k::bigint)||int8send(0::bigint)||int8send(0::bigint)||int8send(0::bigint),
			       '\x08'::bytea, 'sku-'||lpad(k::text,8,'0'), 'product '||k,
			       'cat-'||lpad((k%%1000)::text,4,'0'),
			       ((k%%90000)+1000)::numeric/100, timestamptz '2020-01-01'
			FROM generate_series(0,$1-1) k`, tbl), *flagRows))
	}
	insert("s5p.v_part")
	insert("s5p.v_flat")
	must(exec(ctx, pool, `CREATE INDEX ON s5p.v_part (branch_id, seq_from, seq_to)`))
	must(exec(ctx, pool, `CREATE INDEX ON s5p.v_flat (branch_id, seq_from, seq_to)`))
	must(exec(ctx, pool, `ANALYZE s5p.v_part; ANALYZE s5p.v_flat;`))

	dropParts := *flagParts * *flagDropPct / 100
	if dropParts < 1 {
		dropParts = 1
	}
	cutoff := dropParts * rowsPerPart

	t0 := time.Now()
	for i := 0; i < dropParts; i++ {
		must(exec(ctx, pool, fmt.Sprintf(`DROP TABLE s5p.v_part_%d`, i)))
	}
	dropDur := time.Since(t0)

	t0 = time.Now()
	must(exec(ctx, pool, `DELETE FROM s5p.v_flat WHERE seq_from < $1`, cutoff))
	deleteDur := time.Since(t0)

	// A DELETE has not reclaimed anything until VACUUM runs; a partition DROP
	// reclaims immediately. Counting only the DELETE understates the gap.
	t0 = time.Now()
	must(exec(ctx, pool, `VACUUM s5p.v_flat`))
	vacuumDur := time.Since(t0)

	fmt.Printf("dropping %d of %d partitions (%d rows):\n", dropParts, *flagParts, cutoff)
	fmt.Printf("  DROP PARTITION:            %v\n", dropParts2(dropDur))
	fmt.Printf("  DELETE:                    %v\n", dropParts2(deleteDur))
	fmt.Printf("  DELETE + VACUUM:           %v\n", dropParts2(deleteDur+vacuumDur))
	fmt.Printf("\n  speedup vs DELETE:         %.1fx\n", float64(deleteDur)/float64(dropDur))
	fmt.Printf("  speedup vs DELETE+VACUUM:  %.1fx\n", float64(deleteDur+vacuumDur)/float64(dropDur))
}

func dropParts2(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

func exec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
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
	case "sizes":
		sizes(ctx, pool)
	case "pruning":
		pruning(ctx, pool)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode")
		os.Exit(2)
	}
}
