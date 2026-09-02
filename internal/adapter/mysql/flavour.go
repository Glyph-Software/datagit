package mysql

import (
	"context"
	"fmt"
	"strings"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// Quote renders an identifier. MySQL uses backticks unless ANSI_QUOTES is set,
// and DataGit does not set session-wide SQL modes on a database it does not own.
func (a *Adapter) Quote(ident string) string { return quoteIdent(ident) }

// InsertOnConflict renders MySQL's upsert.
//
// Two deliberate choices:
//
// The row alias form (`AS new`) is used rather than VALUES(col), which MySQL 8.0
// deprecated. MySQL 8.4 is the target (§M0.2), so the modern form is available.
//
// "Do nothing" is a no-op ON DUPLICATE KEY UPDATE rather than INSERT IGNORE.
// INSERT IGNORE downgrades EVERY error to a warning, so a truncated value or a
// bad date would be silently accepted; the PostgreSQL clause it is standing in
// for ignores only the duplicate key. Matching the narrower meaning is the whole
// point of having an adapter.
func (a *Adapter) InsertOnConflict(table string, cols []string, body string,
	conflictCols, updateCols []string) string {

	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}

	// The alias is only legal on a VALUES body; on INSERT ... SELECT the SELECT
	// already names its own sources and MySQL rejects the row alias.
	isSelect := strings.HasPrefix(strings.TrimSpace(strings.ToUpper(body)), "SELECT")

	var action string
	switch {
	case len(updateCols) == 0:
		// A self-assignment: syntactically an update, semantically nothing. It
		// reports 0 rows affected, exactly like DO NOTHING.
		action = fmt.Sprintf("%s = %s", quoteIdent(cols[0]), quoteIdent(cols[0]))
	case isSelect:
		// Without an alias the only way to reach the proposed row is VALUES(col),
		// deprecated but still functional, and the sole option for this shape.
		sets := make([]string, len(updateCols))
		for i, c := range updateCols {
			sets[i] = fmt.Sprintf("%s = VALUES(%s)", quoteIdent(c), quoteIdent(c))
		}
		action = strings.Join(sets, ", ")
	default:
		sets := make([]string, len(updateCols))
		for i, c := range updateCols {
			sets[i] = fmt.Sprintf("%s = new.%s", quoteIdent(c), quoteIdent(c))
		}
		action = strings.Join(sets, ", ")
	}

	alias := ""
	if !isSelect && len(updateCols) > 0 {
		alias = " AS new"
	}
	return fmt.Sprintf("INSERT INTO %s (%s) %s%s ON DUPLICATE KEY UPDATE %s",
		quoteIdent(table), strings.Join(q, ", "), body, alias, action)
}

// InsertReturningID reads the generated key back with LAST_INSERT_ID().
//
// This is connection-scoped, not global, and a transaction in database/sql is
// pinned to one connection, so the value cannot be another writer's. It is read
// in the same transaction as the insert for the same reason.
func (a *Adapter) InsertReturningID(ctx context.Context, tx adapter.Tx,
	sql string, args ...any) (int64, error) {

	n, err := tx.ExecCount(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	// LAST_INSERT_ID() keeps the PREVIOUS statement's value when a statement
	// inserts nothing, so an INSERT ... SELECT that matched no rows would
	// otherwise hand back some earlier row's id. The affected-row count is what
	// distinguishes the two, and 0 matches what RETURNING yields on PostgreSQL.
	if n == 0 {
		return 0, nil
	}
	var id int64
	if err := tx.QueryRow(ctx, "SELECT LAST_INSERT_ID()").Scan(&id); err != nil {
		return 0, fmt.Errorf("mysql: reading the generated id: %w", err)
	}
	return id, nil
}
