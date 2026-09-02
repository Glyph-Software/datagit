// Package db is the connection boundary: everything above it is engine-neutral.
//
// The portability strategy is deliberate and worth stating, because the
// alternative is tempting and worse.
//
// Store code writes ONE dialect of SQL — PostgreSQL-flavoured, with $N
// placeholders and double-quoted identifiers — and this layer translates it
// mechanically for MySQL. The alternative, branching every query on the engine,
// doubles the number of statements that can be wrong and halves the chance
// either version is exercised. A single statement that runs on both engines is a
// statement whose behaviour is tested twice.
//
// Mechanical translation is only valid where the engines differ in SPELLING.
// Where they differ in SEMANTICS — upsert, RETURNING, data-modifying CTEs —
// there is nothing to translate, and those go through explicit adapter methods
// instead (§4.3). This layer never guesses at a semantic difference.
package db

import (
	"context"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// Pool is the engine-neutral connection pool.
type Pool interface {
	// InTx runs fn in a transaction, committing on success and rolling back on
	// any error or panic.
	InTx(ctx context.Context, fn func(adapter.Tx) error) error
	// Direct is a non-transactional handle, for read-only queries and for DDL
	// that cannot run inside a transaction.
	Direct() adapter.Tx
	Dialect() adapter.Dialect
	Close()
}
