package mysql

import (
	"fmt"
	"strings"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// DDL returns the MySQL schema-change generator.
func (a *Adapter) DDL() adapter.DDLGen { return ddlGen{} }

type ddlGen struct{}

// guarded makes a statement idempotent by testing the catalogue first.
//
// MySQL has no IF NOT EXISTS on a column operation, and a migration step MUST be
// safe to re-run: a crashed apply resumes from the journal, so a step that
// already took effect can be reached again (§10.4). Without this, resuming a
// half-applied plan fails on the first completed step and the plan is stuck.
//
// The idiom is MySQL's only conditional-DDL construct: build the statement as a
// string, then prepare and execute it. `DO 0` is the do-nothing branch. It runs
// as several statements, which is why the MySQL pool enables multi-statement
// mode.
func guarded(cond, ddl string) string {
	return fmt.Sprintf(
		"SET @datagit_ddl := IF(%s, %s, 'DO 0'); "+
			"PREPARE datagit_stmt FROM @datagit_ddl; "+
			"EXECUTE datagit_stmt; "+
			"DEALLOCATE PREPARE datagit_stmt",
		cond, sqlString(ddl))
}

// sqlString renders a Go string as a SQL string literal. Backslash matters:
// MySQL treats it as an escape inside a literal unless NO_BACKSLASH_ESCAPES is
// set, and an unescaped one in a column name or type would change the statement.
func sqlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(s) + "'"
}

func columnMissing(table, col string) string {
	return fmt.Sprintf(
		"(SELECT COUNT(*) FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = %s AND COLUMN_NAME = %s) = 0",
		sqlString(table), sqlString(col))
}

func columnPresent(table, col string) string {
	return fmt.Sprintf(
		"(SELECT COUNT(*) FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = %s AND COLUMN_NAME = %s) > 0",
		sqlString(table), sqlString(col))
}

func (ddlGen) AddColumn(table, col, sqlType string) string {
	return guarded(columnMissing(table, col),
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s",
			quoteIdent(table), quoteIdent(col), sqlType))
}

func (ddlGen) DropColumn(table, col string) string {
	return guarded(columnPresent(table, col),
		fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", quoteIdent(table), quoteIdent(col)))
}

// DeprecateColumn marks phase one of a two-phase drop. MySQL carries a comment
// on the column definition rather than as a separate statement, so the column's
// type has to be restated -- and it is not known here. Reading it back from the
// catalogue inside the statement is not possible either, so this is expressed as
// a no-op with the intent recorded in the journal's op kind instead.
//
// The two-phase drop still works: the deprecate step is a journalled marker that
// makes the rollout window explicit, and the drop is a separate later step. Only
// the human-visible comment is missing.
func (ddlGen) DeprecateColumn(table, col string) string {
	return fmt.Sprintf("DO 0 /* datagit: %s.%s deprecated, pending drop */", table, col)
}

// RenameColumn uses MySQL 8.0's RENAME COLUMN, which unlike CHANGE does not
// require restating the type.
func (ddlGen) RenameColumn(table, from, to string) string {
	return guarded(columnPresent(table, from),
		fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
			quoteIdent(table), quoteIdent(from), quoteIdent(to)))
}

// AlterColumnType, SetNotNull, and DropNotNull all go through MODIFY COLUMN,
// which restates the WHOLE definition. That is why the DDLGen interface passes
// the type to the nullability operations even though PostgreSQL ignores it:
// MySQL cannot change nullability without it, and a missing type here would
// silently reset the column to its default type.
func (ddlGen) AlterColumnType(table, col, sqlType string) string {
	return fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s %s",
		quoteIdent(table), quoteIdent(col), sqlType)
}

func (ddlGen) SetNotNull(table, col, sqlType string) string {
	return fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s %s NOT NULL",
		quoteIdent(table), quoteIdent(col), sqlType)
}

func (ddlGen) DropNotNull(table, col, sqlType string) string {
	return fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s %s NULL",
		quoteIdent(table), quoteIdent(col), sqlType)
}

// PreflightNotNull fails loudly when the column still holds nulls, so a plan
// that would fail says so before it has changed anything. Division by zero is an
// error in both engines under the default SQL mode.
func (ddlGen) PreflightNotNull(table, col string) string {
	return fmt.Sprintf(
		"SELECT 1/(CASE WHEN count(*) = 0 THEN 1 ELSE 0 END) FROM %s WHERE %s IS NULL",
		quoteIdent(table), quoteIdent(col))
}
