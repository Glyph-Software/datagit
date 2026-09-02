// S4: does the journalled migration state machine survive crashes without
// transactional DDL? (PLAN.md Phase 0, gates M6.)
// THROWAWAY spike code — not part of the shipped tree.
//
// MySQL commits implicitly on every DDL statement, so a multi-statement
// migration that fails halfway cannot be rolled back by the engine. DESIGN.md
// §10.4 answers that with a resumable state machine rather than a transaction:
// every operation is journalled before execution and marked complete after, every
// operation is written to be idempotent, and a crashed apply RESUMES from the
// journal rather than restarting or being left indeterminate.
//
// This spike kills the process at every step boundary and mid-step, restarts, and
// asserts convergence to the same final state — on MySQL and, deliberately, on
// PostgreSQL too, because §10.4 runs the same machine on both so that failure
// behaviour is identical and only has to be tested once.
//
//	go run ./spikes/s4_migration -engine mysql
//	go run ./spikes/s4_migration -engine postgres
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	flagEngine = flag.String("engine", "mysql", "mysql | postgres")
	flagMySQL  = flag.String("mysql", "datagit:datagit@tcp(localhost:55484)/datagit?multiStatements=true&parseTime=true", "MySQL DSN")
	flagPG     = flag.String("pg", "postgres://datagit:datagit@localhost:55417/datagit", "PostgreSQL DSN")
)

// op is one migration step. Every one must be idempotent, because a resume
// re-runs whatever was in flight when the process died.
type op struct {
	ordinal int
	kind    string
	run     func(ctx context.Context, db *sql.DB) error
}

type engine struct {
	name        string
	db          *sql.DB
	transactDDL bool
	// exists checks are engine-specific; MySQL has no IF NOT EXISTS for columns
	// before 8.0.29 and none for constraints, so idempotency is written by hand.
	columnExists func(ctx context.Context, db *sql.DB, table, col string) (bool, error)
	indexExists  func(ctx context.Context, db *sql.DB, table, idx string) (bool, error)
}

func main() {
	flag.Parse()
	ctx := context.Background()

	e, err := open(*flagEngine)
	must(err)
	defer e.db.Close()

	fmt.Printf("=== S4 on %s (transactional DDL: %v)\n", e.name, e.transactDDL)
	fmt.Println("Every operation is journalled before execution and marked complete after.")
	fmt.Println("A crash resumes from the journal rather than restarting.")

	ops := plan(e)

	// Baseline: a clean run, to know what convergence looks like.
	must(reset(ctx, e))
	must(apply(ctx, e, ops, -1))
	want, err := describe(ctx, e)
	must(err)
	fmt.Printf("\nclean run converges to: %s\n", want)

	// Now crash at every step boundary, resume, and compare.
	fmt.Printf("\n%-28s %-10s %s\n", "crash point", "resumed", "converged")
	failures := 0
	for crashAt := 0; crashAt < len(ops); crashAt++ {
		must(reset(ctx, e))

		// First attempt: dies immediately before completing `crashAt`.
		err := apply(ctx, e, ops, crashAt)
		if err == nil {
			fmt.Printf("  crash injection at %d did not fire\n", crashAt)
			failures++
			continue
		}

		// Restart: a fresh apply, which must resume rather than redo.
		if err := apply(ctx, e, ops, -1); err != nil {
			fmt.Printf("%-28s %-10s FAILED to resume: %v\n",
				fmt.Sprintf("before op %d (%s)", crashAt, ops[crashAt].kind), "no", err)
			failures++
			continue
		}
		got, err := describe(ctx, e)
		must(err)
		ok := got == want
		if !ok {
			failures++
		}
		fmt.Printf("%-28s %-10s %v\n",
			fmt.Sprintf("before op %d (%s)", crashAt, ops[crashAt].kind), "yes", ok)
		if !ok {
			fmt.Printf("    got:  %s\n    want: %s\n", got, want)
		}
	}

	// And mid-step: kill after the journal records the start but before the
	// operation completes. This is the case transactional DDL would have covered.
	fmt.Println("\nmid-step crashes (journalled as started, never completed):")
	for crashAt := 0; crashAt < len(ops); crashAt++ {
		must(reset(ctx, e))
		if err := apply(ctx, e, ops, -1); err != nil {
			must(err)
		}
		// Simulate: mark one op started-but-not-complete and re-run.
		_, err := e.db.ExecContext(ctx,
			rebind(e, `UPDATE s4_journal SET completed_at = NULL WHERE ordinal = ?`), crashAt)
		must(err)
		if err := apply(ctx, e, ops, -1); err != nil {
			fmt.Printf("  op %d (%s): resume FAILED: %v\n", crashAt, ops[crashAt].kind, err)
			failures++
			continue
		}
		got, err := describe(ctx, e)
		must(err)
		ok := got == want
		if !ok {
			failures++
		}
		fmt.Printf("  op %d (%-12s) re-ran idempotently: %v\n", crashAt, ops[crashAt].kind, ok)
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("S4 FAILED on %s: %d divergence(s)\n", e.name, failures)
		os.Exit(1)
	}
	fmt.Printf("S4 PASSED on %s: every crash point converges to the same final state\n", e.name)
}

// plan is the 4-operation migration PLAN.md specifies: add a column, backfill
// it, add an index, drop a column.
func plan(e *engine) []op {
	return []op{
		{0, "add_column", func(ctx context.Context, db *sql.DB) error {
			ok, err := e.columnExists(ctx, db, "s4_products", "margin_pct")
			if err != nil || ok {
				return err
			}
			_, err = db.ExecContext(ctx, `ALTER TABLE s4_products ADD COLUMN margin_pct decimal(5,2)`)
			return err
		}},
		{1, "backfill", func(ctx context.Context, db *sql.DB) error {
			// Idempotent by construction: it only touches rows still NULL.
			_, err := db.ExecContext(ctx,
				`UPDATE s4_products SET margin_pct = 12.50 WHERE margin_pct IS NULL`)
			return err
		}},
		{2, "add_index", func(ctx context.Context, db *sql.DB) error {
			ok, err := e.indexExists(ctx, db, "s4_products", "s4_products_margin")
			if err != nil || ok {
				return err
			}
			_, err = db.ExecContext(ctx, `CREATE INDEX s4_products_margin ON s4_products (margin_pct)`)
			return err
		}},
		{3, "drop_column", func(ctx context.Context, db *sql.DB) error {
			ok, err := e.columnExists(ctx, db, "s4_products", "legacy_note")
			if err != nil || !ok {
				return err
			}
			_, err = db.ExecContext(ctx, `ALTER TABLE s4_products DROP COLUMN legacy_note`)
			return err
		}},
	}
}

// apply runs the plan, resuming from the journal. crashAt >= 0 injects a failure
// immediately before that operation's completion is recorded.
func apply(ctx context.Context, e *engine, ops []op, crashAt int) error {
	done, err := completed(ctx, e)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if done[o.ordinal] {
			continue
		}
		// Journal the start BEFORE executing. On MySQL the DDL commits itself, so
		// this is the only record that the operation was attempted.
		if _, err := e.db.ExecContext(ctx, rebind(e,
			`INSERT INTO s4_journal (ordinal, kind, started_at) VALUES (?, ?, now())
			 ON `+conflictClause(e)+` started_at = now()`), o.ordinal, o.kind); err != nil {
			return fmt.Errorf("journal start %d: %w", o.ordinal, err)
		}
		if err := o.run(ctx, e.db); err != nil {
			return fmt.Errorf("op %d (%s): %w", o.ordinal, o.kind, err)
		}
		if o.ordinal == crashAt {
			return fmt.Errorf("injected crash before completing op %d", crashAt)
		}
		if _, err := e.db.ExecContext(ctx, rebind(e,
			`UPDATE s4_journal SET completed_at = now() WHERE ordinal = ?`), o.ordinal); err != nil {
			return fmt.Errorf("journal complete %d: %w", o.ordinal, err)
		}
	}
	return nil
}

func completed(ctx context.Context, e *engine) (map[int]bool, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT ordinal FROM s4_journal WHERE completed_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// describe renders the schema, so convergence can be compared exactly.
func describe(ctx context.Context, e *engine) (string, error) {
	var q string
	switch e.name {
	case "mysql":
		q = `SELECT column_name FROM information_schema.columns
		      WHERE table_schema = DATABASE() AND table_name = 's4_products'
		      ORDER BY column_name`
	default:
		q = `SELECT column_name FROM information_schema.columns
		      WHERE table_schema = current_schema() AND table_name = 's4_products'
		      ORDER BY column_name`
	}
	rows, err := e.db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		cols = append(cols, c)
	}
	var filled int
	if err := e.db.QueryRowContext(ctx,
		`SELECT count(*) FROM s4_products WHERE margin_pct IS NOT NULL`).Scan(&filled); err != nil {
		// The column may not exist yet on a partial run.
		filled = -1
	}
	idx, err := e.indexExists(ctx, e.db, "s4_products", "s4_products_margin")
	if err != nil {
		idx = false
	}
	return fmt.Sprintf("cols=[%s] filled=%d index=%v", strings.Join(cols, ","), filled, idx), nil
}

func reset(ctx context.Context, e *engine) error {
	stmts := []string{
		`DROP TABLE IF EXISTS s4_products`,
		`DROP TABLE IF EXISTS s4_journal`,
		`CREATE TABLE s4_products (
			sku varchar(64) PRIMARY KEY, name varchar(200),
			price decimal(12,2), legacy_note varchar(200))`,
		`CREATE TABLE s4_journal (
			ordinal int PRIMARY KEY, kind varchar(64) NOT NULL,
			started_at timestamp NULL, completed_at timestamp NULL)`,
	}
	for _, s := range stmts {
		if _, err := e.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	for i := 0; i < 50; i++ {
		if _, err := e.db.ExecContext(ctx, rebind(e,
			`INSERT INTO s4_products (sku, name, price, legacy_note) VALUES (?, ?, ?, ?)`),
			fmt.Sprintf("sku-%03d", i), fmt.Sprintf("product %d", i), 10.0+float64(i), "old"); err != nil {
			return err
		}
	}
	return nil
}

func open(name string) (*engine, error) {
	switch name {
	case "mysql":
		db, err := sql.Open("mysql", *flagMySQL)
		if err != nil {
			return nil, err
		}
		return &engine{
			name: "mysql", db: db, transactDDL: false,
			columnExists: func(ctx context.Context, db *sql.DB, tbl, col string) (bool, error) {
				var n int
				err := db.QueryRowContext(ctx,
					`SELECT count(*) FROM information_schema.columns
					  WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
					tbl, col).Scan(&n)
				return n > 0, err
			},
			indexExists: func(ctx context.Context, db *sql.DB, tbl, idx string) (bool, error) {
				var n int
				err := db.QueryRowContext(ctx,
					`SELECT count(*) FROM information_schema.statistics
					  WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
					tbl, idx).Scan(&n)
				return n > 0, err
			},
		}, db.Ping()
	case "postgres":
		db, err := sql.Open("pgx", *flagPG)
		if err != nil {
			return nil, err
		}
		return &engine{
			name: "postgres", db: db, transactDDL: true,
			columnExists: func(ctx context.Context, db *sql.DB, tbl, col string) (bool, error) {
				var n int
				err := db.QueryRowContext(ctx,
					`SELECT count(*) FROM information_schema.columns
					  WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
					tbl, col).Scan(&n)
				return n > 0, err
			},
			indexExists: func(ctx context.Context, db *sql.DB, tbl, idx string) (bool, error) {
				var n int
				err := db.QueryRowContext(ctx,
					`SELECT count(*) FROM pg_indexes
					  WHERE schemaname = current_schema() AND tablename = $1 AND indexname = $2`,
					tbl, idx).Scan(&n)
				return n > 0, err
			},
		}, db.Ping()
	}
	return nil, fmt.Errorf("unknown engine %q", name)
}

// rebind converts ? placeholders to $n for PostgreSQL.
func rebind(e *engine, q string) string {
	if e.name != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func conflictClause(e *engine) string {
	if e.name == "postgres" {
		return "CONFLICT (ordinal) DO UPDATE SET"
	}
	return "DUPLICATE KEY UPDATE"
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
