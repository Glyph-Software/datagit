package postgres

import (
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// DDL returns the PostgreSQL schema-change generator.
func (a *Adapter) DDL() adapter.DDLGen { return ddlGen{} }

type ddlGen struct{}

// PostgreSQL expresses idempotency inline, so every statement here is a single
// one that is safe to re-run.

func (ddlGen) AddColumn(table, col, sqlType string) string {
	return fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`,
		quoteIdent(table), quoteIdent(col), sqlType)
}

func (ddlGen) DropColumn(table, col string) string {
	return fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`,
		quoteIdent(table), quoteIdent(col))
}

func (ddlGen) DeprecateColumn(table, col string) string {
	return fmt.Sprintf(`COMMENT ON COLUMN %s.%s IS 'datagit: deprecated, pending drop'`,
		quoteIdent(table), quoteIdent(col))
}

func (ddlGen) RenameColumn(table, from, to string) string {
	return fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`,
		quoteIdent(table), quoteIdent(from), quoteIdent(to))
}

func (ddlGen) AlterColumnType(table, col, sqlType string) string {
	return fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE %s`,
		quoteIdent(table), quoteIdent(col), sqlType)
}

// SetNotNull and DropNotNull ignore the type: PostgreSQL alters nullability on
// its own, without restating the column definition.
func (ddlGen) SetNotNull(table, col, _ string) string {
	return fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET NOT NULL`,
		quoteIdent(table), quoteIdent(col))
}

func (ddlGen) DropNotNull(table, col, _ string) string {
	return fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL`,
		quoteIdent(table), quoteIdent(col))
}

func (ddlGen) PreflightNotNull(table, col string) string {
	return fmt.Sprintf(
		`SELECT 1/(CASE WHEN count(*) = 0 THEN 1 ELSE 0 END) FROM %s WHERE %s IS NULL`,
		quoteIdent(table), quoteIdent(col))
}
