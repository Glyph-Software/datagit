// Package core holds the value and change types shared by the reference model
// (internal/model) and the real implementation (internal/resolve, internal/merge).
//
// Only *data* lives here. No resolution, diff, or merge logic: the whole point of
// the differential harness is that the model and the implementation share no
// algorithm, so anything algorithmic in this package would weaken the evidence.
package core

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ColID is a stable column identifier. DESIGN.md §10.5 rule 1: sidecar columns
// are named by immutable id, never by name, so renames are metadata-only and a
// drop-then-re-add yields a new id rather than colliding with old history.
type ColID uint32

type Kind uint8

const (
	KindNull Kind = iota
	KindInt
	KindText
	KindBool
)

// Value is one cell. Deliberately small and comparable by value.
type Value struct {
	Kind Kind
	Int  int64
	Text string
	Bool bool
}

func Null() Value            { return Value{Kind: KindNull} }
func Int(i int64) Value      { return Value{Kind: KindInt, Int: i} }
func Text(s string) Value    { return Value{Kind: KindText, Text: s} }
func Bool(b bool) Value      { return Value{Kind: KindBool, Bool: b} }
func (v Value) IsNull() bool { return v.Kind == KindNull }

// Equal is IS NOT DISTINCT FROM, not SQL `=`.
//
// NULL equals NULL here. This matters for merge: if both branches set a column
// to NULL that is an identical change and merges clean, whereas SQL `=` would
// call it unknown. Every cell comparison in DataGit uses these semantics.
func (v Value) Equal(o Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case KindNull:
		return true
	case KindInt:
		return v.Int == o.Int
	case KindText:
		return v.Text == o.Text
	case KindBool:
		return v.Bool == o.Bool
	}
	return false
}

func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "NULL"
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindText:
		return strconv.Quote(v.Text)
	case KindBool:
		return strconv.FormatBool(v.Bool)
	}
	return "?"
}

// Canonical is the length-prefixed encoding used for primary keys and, later,
// for the commit hash chain (DESIGN.md §12.1). It must stay stable forever once
// history exists; changing it invalidates every commit id ever written.
func (v Value) Canonical() string {
	switch v.Kind {
	case KindNull:
		return "n:"
	case KindInt:
		return "i:" + strconv.FormatInt(v.Int, 10)
	case KindText:
		return "t:" + strconv.Itoa(len(v.Text)) + ":" + v.Text
	case KindBool:
		return "b:" + strconv.FormatBool(v.Bool)
	}
	panic(fmt.Sprintf("core: unknown kind %d", v.Kind))
}

// Row is one row image: every column the schema defines, by stable id.
// DESIGN.md §5.2b stores full row images rather than deltas, so a point read is
// O(1) instead of O(chain depth).
type Row map[ColID]Value

func (r Row) Clone() Row {
	if r == nil {
		return nil
	}
	c := make(Row, len(r))
	for k, v := range r {
		c[k] = v
	}
	return c
}

// Cols returns the column ids in ascending order. Callers must iterate rows
// through this rather than ranging the map directly: Go map order is random and
// would make diffs, masks, and canonical encodings non-deterministic.
func (r Row) Cols() []ColID {
	out := make([]ColID, 0, len(r))
	for c := range r {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Get returns NULL for an absent column so callers never have to distinguish
// "column missing from this image" from "column is NULL".
func (r Row) Get(c ColID) Value {
	if r == nil {
		return Null()
	}
	if v, ok := r[c]; ok {
		return v
	}
	return Null()
}

func (r Row) Equal(o Row) bool {
	if (r == nil) != (o == nil) {
		return false
	}
	seen := map[ColID]bool{}
	for _, c := range r.Cols() {
		if !r.Get(c).Equal(o.Get(c)) {
			return false
		}
		seen[c] = true
	}
	for _, c := range o.Cols() {
		if !seen[c] && !o.Get(c).Equal(r.Get(c)) {
			return false
		}
	}
	return true
}

func (r Row) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, c := range r.Cols() {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d=%s", c, r[c])
	}
	b.WriteByte('}')
	return b.String()
}

// PK is the canonical encoding of a row's primary key tuple. DESIGN.md §3.2:
// a row's identity is its primary key, for all of history.
type PK string

// MakePK builds the canonical key from the pk columns, in the schema's declared
// order. Order is part of the encoding, so it must come from the schema and not
// from map iteration.
func MakePK(r Row, pkCols []ColID) PK {
	var b strings.Builder
	for i, c := range pkCols {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(r.Get(c).Canonical())
	}
	return PK(b.String())
}
