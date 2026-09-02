// Package my adapts database/sql and go-sql-driver to the adapter.Tx interface,
// translating PostgreSQL-flavoured SQL on the way through (§4.3).
//
// Three translations happen here, all of them mechanical:
//
//   - $N placeholders become ?, with the arguments permuted to match.
//   - "double-quoted" identifiers become `backticked` ones.
//   - Fixed-size byte arrays (UUIDs, commit digests) become []byte on the way in
//     and are copied back on the way out.
//
// The third exists because pgx maps Go's [16]byte to uuid natively and
// database/sql does not: uuid.UUID's own Value() renders the 36-character text
// form, which would silently write text into a BINARY(16) column. Converting
// here keeps that difference out of every call site.
package my

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/db"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// Pool wraps a database/sql pool.
type Pool struct{ db *sql.DB }

func Open(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("my: parse dsn: %w", err)
	}
	// ParseTime makes DATETIME columns scan into time.Time, matching pgx and
	// keeping timestamp handling identical above this layer. UTC keeps
	// committed_at comparable across replicas in different zones (§7.2).
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	// Multi-statement is needed for the control-schema DDL, which is one script.
	cfg.MultiStatements = true

	h, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("my: open: %w", err)
	}
	h.SetMaxOpenConns(32)
	h.SetMaxIdleConns(8)
	h.SetConnMaxLifetime(time.Hour)
	if err := h.PingContext(ctx); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("my: ping: %w", err)
	}
	return &Pool{db: h}, nil
}

func (p *Pool) Close()                   { _ = p.db.Close() }
func (p *Pool) Dialect() adapter.Dialect { return adapter.MySQL }
func (p *Pool) Direct() adapter.Tx       { return &conn{q: p.db} }
func (p *Pool) DB() *sql.DB              { return p.db }

// InTx runs fn in a transaction. REPEATABLE READ is MySQL's default and is what
// the design assumes for a consistent read of the resolution chain (§11.1).
func (p *Pool) InTx(ctx context.Context, fn func(adapter.Tx) error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("my: begin: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()
	if err := fn(&conn{q: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// querier is the shared shape of *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

type conn struct{ q querier }

// translate applies the three mechanical rewrites.
func translate(q string, args []any) (string, []any) {
	q = db.QuoteToBacktick(q)
	q, order := db.Rebind(q)
	return q, convertArgs(db.Reorder(args, order))
}

func (c *conn) Exec(ctx context.Context, q string, args ...any) error {
	_, err := c.ExecCount(ctx, q, args...)
	return err
}

func (c *conn) ExecCount(ctx context.Context, q string, args ...any) (int64, error) {
	q, a := translate(q, args)
	res, err := c.q.ExecContext(ctx, q, a...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the driver does not report it; not an error for callers
	}
	return n, nil
}

func (c *conn) Query(ctx context.Context, q string, args ...any) (adapter.Rows, error) {
	q, a := translate(q, args)
	rows, err := c.q.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return &myRows{r: rows}, nil
}

func (c *conn) QueryRow(ctx context.Context, q string, args ...any) adapter.Row {
	q, a := translate(q, args)
	return &myRow{r: c.q.QueryRowContext(ctx, q, a...)}
}

type myRows struct{ r *sql.Rows }

func (r *myRows) Next() bool { return r.r.Next() }
func (r *myRows) Scan(dest ...any) error {
	holders, finish := convertDest(dest)
	if err := r.r.Scan(holders...); err != nil {
		return err
	}
	return finish()
}
func (r *myRows) Close()     { _ = r.r.Close() }
func (r *myRows) Err() error { return r.r.Err() }

type myRow struct{ r *sql.Row }

func (r *myRow) Scan(dest ...any) error {
	holders, finish := convertDest(dest)
	if err := r.r.Scan(holders...); err != nil {
		return err
	}
	return finish()
}

// convertArgs turns fixed-size byte arrays into slices the driver understands.
func convertArgs(args []any) []any {
	for i, a := range args {
		switch v := a.(type) {
		case uuid.UUID:
			b := make([]byte, 16)
			copy(b, v[:])
			args[i] = b
		case [16]byte:
			b := make([]byte, 16)
			copy(b, v[:])
			args[i] = b
		case hash.Digest:
			b := make([]byte, len(v))
			copy(b, v[:])
			args[i] = b
		case []uuid.UUID:
			// Rendered by callers as an IN list; a slice argument would be a bug.
			args[i] = v
		}
	}
	return args
}

// convertDest supplies scannable holders for destinations database/sql cannot
// fill directly, and copies the bytes back afterwards.
func convertDest(dest []any) ([]any, func() error) {
	var fixups []func() error
	out := make([]any, len(dest))
	for i, d := range dest {
		switch p := d.(type) {
		case *uuid.UUID:
			var raw []byte
			out[i] = &raw
			target := p
			fixups = append(fixups, func() error { return copyFixed(target[:], raw, 16) })
		case *[16]byte:
			var raw []byte
			out[i] = &raw
			target := p
			fixups = append(fixups, func() error { return copyFixed(target[:], raw, 16) })
		case *hash.Digest:
			var raw []byte
			out[i] = &raw
			target := p
			fixups = append(fixups, func() error {
				if len(raw) == 0 {
					*target = hash.Digest{}
					return nil
				}
				return copyFixed(target[:], raw, hash.Size)
			})
		default:
			out[i] = d
		}
	}
	if len(fixups) == 0 {
		return out, func() error { return nil }
	}
	return out, func() error {
		for _, f := range fixups {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	}
}

func copyFixed(dst, src []byte, want int) error {
	if len(src) == 0 {
		for i := range dst {
			dst[i] = 0
		}
		return nil
	}
	if len(src) != want {
		return fmt.Errorf("my: expected %d bytes, got %d", want, len(src))
	}
	copy(dst, src)
	return nil
}
