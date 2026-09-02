// Package connect chooses an engine from a DSN and returns a matched pool and
// adapter.
//
// It exists so no caller has to name an engine. A binary given a MySQL DSN and a
// PostgreSQL adapter would build, start, and fail on its first statement; making
// the pair impossible to mismatch is worth one package.
package connect

import (
	"context"
	"fmt"
	"strings"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/mysql"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/db"
	"github.com/Glyph-Software/datagit/internal/my"
	"github.com/Glyph-Software/datagit/internal/pg"
)

// Open connects and returns the pool with the adapter that matches it.
func Open(ctx context.Context, dsn string) (db.Pool, adapter.Adapter, error) {
	switch Detect(dsn) {
	case adapter.MySQL:
		pool, err := my.Open(ctx, strings.TrimPrefix(dsn, "mysql://"))
		if err != nil {
			return nil, nil, err
		}
		// The exec hook lets the adapter run DDL outside a transaction, which is
		// the only way to run it on MySQL: DDL there commits implicitly, so a
		// migration step inside a transaction would silently end it (§4.3).
		return pool, mysql.NewWithExec(func(ctx context.Context, sql string) error {
			return pool.Direct().Exec(ctx, sql)
		}), nil
	default:
		pool, err := pg.Open(ctx, dsn)
		if err != nil {
			return nil, nil, err
		}
		return pool, postgres.NewWithExec(func(ctx context.Context, sql string) error {
			return pool.Direct().Exec(ctx, sql)
		}), nil
	}
}

// Detect names the engine a DSN addresses.
//
// An explicit scheme wins. Failing that, the go-sql-driver network form
// (user:pass@tcp(host)/db) is unambiguous, and everything else is PostgreSQL,
// which also accepts the bare key=value form that has no scheme to inspect.
func Detect(dsn string) adapter.Dialect {
	l := strings.ToLower(dsn)
	switch {
	case strings.HasPrefix(l, "mysql://"):
		return adapter.MySQL
	case strings.HasPrefix(l, "postgres://"), strings.HasPrefix(l, "postgresql://"):
		return adapter.PostgreSQL
	case strings.Contains(l, "@tcp("), strings.Contains(l, "@unix("):
		return adapter.MySQL
	default:
		return adapter.PostgreSQL
	}
}

// Describe names the engine for an error or a log line.
func Describe(d adapter.Dialect) string {
	switch d {
	case adapter.MySQL:
		return "MySQL"
	case adapter.PostgreSQL:
		return "PostgreSQL"
	default:
		return fmt.Sprintf("%s", string(d))
	}
}
