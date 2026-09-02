// Package pg adapts pgx to the narrow adapter.Tx interface.
//
// The interface is deliberately small so the engine and store packages can be
// tested with fakes and so swapping the driver does not reach into them.
package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// Pool wraps a pgx pool.
type Pool struct{ p *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse dsn: %w", err)
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: connect: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return &Pool{p: p}, nil
}

func (p *Pool) Close() { p.p.Close() }

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
func (p *Pool) InTx(ctx context.Context, fn func(adapter.Tx) error) error {
	return pgx.BeginFunc(ctx, p.p, func(tx pgx.Tx) error { return fn(&Tx{tx: tx}) })
}

// Direct exposes a non-transactional handle, for DDL that cannot run inside a
// transaction and for read-only queries.
func (p *Pool) Direct() adapter.Tx { return &poolTx{p: p.p} }

type Tx struct{ tx pgx.Tx }

func (t *Tx) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := t.tx.Exec(ctx, sql, args...)
	return err
}

func (t *Tx) Query(ctx context.Context, sql string, args ...any) (adapter.Rows, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows}, nil
}

func (t *Tx) QueryRow(ctx context.Context, sql string, args ...any) adapter.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

type poolTx struct{ p *pgxpool.Pool }

func (t *poolTx) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := t.p.Exec(ctx, sql, args...)
	return err
}

func (t *poolTx) Query(ctx context.Context, sql string, args ...any) (adapter.Rows, error) {
	rows, err := t.p.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows}, nil
}

func (t *poolTx) QueryRow(ctx context.Context, sql string, args ...any) adapter.Row {
	return t.p.QueryRow(ctx, sql, args...)
}

type pgxRows struct{ r pgx.Rows }

func (r *pgxRows) Next() bool                { return r.r.Next() }
func (r *pgxRows) Scan(dest ...any) error    { return r.r.Scan(dest...) }
func (r *pgxRows) Close()                    { r.r.Close() }
func (r *pgxRows) Err() error                { return r.r.Err() }
