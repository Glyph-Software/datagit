package mysql

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
)

// KindFor reports the canonical kind for a MySQL type.
func (a *Adapter) KindFor(sqlType string) (core.Kind, bool) { return KindFor(sqlType) }

// Introspect reads a live table's columns and primary key from
// information_schema, which is MySQL's only catalogue.
//
// COLUMN_TYPE rather than DATA_TYPE, because DATA_TYPE drops the parts that
// decide the canonical kind: it reports "decimal" without the precision and
// scale, and "int" without the unsigned flag. An unsigned bigint does not fit a
// signed 64-bit integer, so losing that distinction would silently corrupt a
// value on its way into the canonical encoding.
func (a *Adapter) Introspect(ctx context.Context, tx adapter.Tx, physical string) (
	[]adapter.Column, []core.ColID, error) {

	rows, err := tx.Query(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, ORDINAL_POSITION
		  FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = $1
		 ORDER BY ORDINAL_POSITION`, physical)
	if err != nil {
		return nil, nil, fmt.Errorf("introspect %q: %w", physical, err)
	}
	defer rows.Close()

	var cols []adapter.Column
	byName := map[string]core.ColID{}
	next := core.ColID(1)
	for rows.Next() {
		var name, typ, nullable string
		var pos int64
		if err := rows.Scan(&name, &typ, &nullable, &pos); err != nil {
			return nil, nil, err
		}
		kind, _ := KindFor(typ)
		cols = append(cols, adapter.Column{
			ID: next, Name: name, SQLType: typ, Kind: kind, Nullable: nullable == "YES",
		})
		byName[name] = next
		next++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf("table %q not found in the current schema", physical)
	}

	// SEQ_IN_INDEX preserves the declared key order, which is part of the key's
	// identity: (a, b) and (b, a) are different primary keys and produce
	// different canonical primary-key encodings (§12.1).
	pkRows, err := tx.Query(ctx, `
		SELECT COLUMN_NAME FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = $1 AND INDEX_NAME = 'PRIMARY'
		 ORDER BY SEQ_IN_INDEX`, physical)
	if err != nil {
		return nil, nil, err
	}
	defer pkRows.Close()
	var pk []core.ColID
	for pkRows.Next() {
		var name string
		if err := pkRows.Scan(&name); err != nil {
			return nil, nil, err
		}
		if id, ok := byName[name]; ok {
			pk = append(pk, id)
		}
	}
	return cols, pk, pkRows.Err()
}

// UniqueIndexes lists unique constraints other than the primary key.
//
// NON_UNIQUE = 0 selects unique indexes, and INDEX_NAME <> 'PRIMARY' excludes
// the primary key, which MySQL reports through the same view.
func (a *Adapter) UniqueIndexes(ctx context.Context, tx adapter.Tx, physical string) (
	[]adapter.UniqueIndex, error) {

	rows, err := tx.Query(ctx, `
		SELECT INDEX_NAME, COLUMN_NAME, SEQ_IN_INDEX
		  FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = $1
		   AND NON_UNIQUE = 0 AND INDEX_NAME <> 'PRIMARY'
		 ORDER BY INDEX_NAME, SEQ_IN_INDEX`, physical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
