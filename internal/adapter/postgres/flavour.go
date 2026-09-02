package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// Quote renders an identifier.
func (a *Adapter) Quote(ident string) string { return quoteIdent(ident) }

// InsertOnConflict renders PostgreSQL's native upsert (§4.3).
func (a *Adapter) InsertOnConflict(table string, cols []string, body string,
	conflictCols, updateCols []string) string {

	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	var target string
	if len(conflictCols) > 0 {
		qc := make([]string, len(conflictCols))
		for i, c := range conflictCols {
			qc[i] = quoteIdent(c)
		}
		target = " (" + strings.Join(qc, ", ") + ")"
	}
	action := "DO NOTHING"
	if len(updateCols) > 0 {
		sets := make([]string, len(updateCols))
		for i, c := range updateCols {
			sets[i] = fmt.Sprintf("%s = EXCLUDED.%s", quoteIdent(c), quoteIdent(c))
		}
		action = "DO UPDATE SET " + strings.Join(sets, ", ")
	}
	return fmt.Sprintf("INSERT INTO %s (%s) %s ON CONFLICT%s %s",
		quoteIdent(table), strings.Join(q, ", "), body, target, action)
}

// InsertReturningID uses RETURNING, which is atomic with the insert.
//
// An INSERT ... SELECT that matched nothing inserts no row and so returns none.
// That is reported as id 0, not as an error, because "the row source was empty"
// is a condition the caller explains far better than a driver's no-rows message.
func (a *Adapter) InsertReturningID(ctx context.Context, tx adapter.Tx,
	sql string, args ...any) (int64, error) {

	var id int64
	if err := tx.QueryRow(ctx, sql+" RETURNING id", args...).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}
