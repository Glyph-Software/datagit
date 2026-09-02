package postgres

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
)

// KindFor reports the canonical kind for a PostgreSQL type.
func (a *Adapter) KindFor(sqlType string) (core.Kind, bool) { return KindFor(sqlType) }

// Introspect reads a live table's columns and primary key from pg_catalog.
//
// Column ids are assigned here, once, and never reused (§10.5 rule 1). They must
// exist from the very first sidecar: retrofitting them later is a full rewrite.
//
// pg_catalog rather than information_schema because attnum gives a stable
// ordinal that survives a dropped column, which is exactly what the id
// assignment needs.
func (a *Adapter) Introspect(ctx context.Context, tx adapter.Tx, physical string) (
	[]adapter.Column, []core.ColID, error) {

	rows, err := tx.Query(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       a.attnum
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relname = $1 AND n.nspname = current_schema()
		   AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY a.attnum`, physical)
	if err != nil {
		return nil, nil, fmt.Errorf("introspect %q: %w", physical, err)
	}
	defer rows.Close()

	var cols []adapter.Column
	byNum := map[int16]core.ColID{}
	next := core.ColID(1)
	for rows.Next() {
		var name, typ string
		var nullable bool
		var attnum int16
		if err := rows.Scan(&name, &typ, &nullable, &attnum); err != nil {
			return nil, nil, err
		}
		kind, _ := KindFor(typ)
		cols = append(cols, adapter.Column{
			ID: next, Name: name, SQLType: typ, Kind: kind, Nullable: nullable,
		})
		byNum[attnum] = next
		next++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf("table %q not found in the current schema", physical)
	}

	pkRows, err := tx.Query(ctx, `
		SELECT unnest(i.indkey)
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relname = $1 AND n.nspname = current_schema() AND i.indisprimary`, physical)
	if err != nil {
		return nil, nil, err
	}
	defer pkRows.Close()
	var pk []core.ColID
	for pkRows.Next() {
		var attnum int16
		if err := pkRows.Scan(&attnum); err != nil {
			return nil, nil, err
		}
		if id, ok := byNum[attnum]; ok {
			pk = append(pk, id)
		}
	}
	return cols, pk, pkRows.Err()
}

// UniqueIndexes lists unique constraints other than the primary key.
func (a *Adapter) UniqueIndexes(ctx context.Context, tx adapter.Tx, physical string) (
	[]adapter.UniqueIndex, error) {

	rows, err := tx.Query(ctx, `
		SELECT i.relname, a.attname, k.ord
		  FROM pg_index x
		  JOIN pg_class c ON c.oid = x.indrelid
		  JOIN pg_class i ON i.oid = x.indexrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  CROSS JOIN LATERAL unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord)
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		 WHERE c.relname = $1 AND n.nspname = current_schema()
		   AND x.indisunique AND NOT x.indisprimary
		 ORDER BY i.relname, k.ord`, physical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectIndexes(rows)
}

// collectIndexes folds (index, column, ordinal) rows into indexes. The rows
// arrive ordered, so the column order each index is declared with is preserved.
func collectIndexes(rows adapter.Rows) ([]adapter.UniqueIndex, error) {
	var out []adapter.UniqueIndex
	for rows.Next() {
		var name, col string
		var ord int64
		if err := rows.Scan(&name, &col, &ord); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, adapter.UniqueIndex{Name: name})
		}
		out[len(out)-1].Cols = append(out[len(out)-1].Cols, col)
	}
	return out, rows.Err()
}
