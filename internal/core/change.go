package core

import (
	"fmt"
	"sort"
)

// Op is the kind of a row version. Values match DESIGN.md §5.2 exactly: the
// sidecar stores these numbers.
type Op uint8

const (
	OpInsert Op = 1
	OpUpdate Op = 2
	OpDelete Op = 3
)

func (o Op) String() string {
	switch o {
	case OpInsert:
		return "insert"
	case OpUpdate:
		return "update"
	case OpDelete:
		return "delete"
	}
	return "?"
}

// Change is one row's worth of a change set.
type Change struct {
	PK      PK
	Op      Op
	Row     Row // nil when Op == OpDelete
	Changed ColMask
}

func (c Change) String() string {
	if c.Op == OpDelete {
		return fmt.Sprintf("- %s", c.PK)
	}
	return fmt.Sprintf("%s %s %s", map[Op]string{OpInsert: "+", OpUpdate: "~"}[c.Op], c.PK, c.Row)
}

// ChangeSet is what a commit carries. DESIGN.md §6.1: a commit is a single
// atomic RPC carrying its whole change set; there is no server-side staging on
// the default branch.
type ChangeSet map[PK]Change

// Sorted returns the changes in primary-key order. Commits apply in this order
// so that two concurrent commits touching overlapping keys take row locks in the
// same sequence and cannot deadlock (DESIGN.md §6.1 property 3).
func (cs ChangeSet) Sorted() []Change {
	out := make([]Change, 0, len(cs))
	for _, c := range cs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PK < out[j].PK })
	return out
}

// ConflictKind classifies a merge conflict. Mirrors the `kind` column of
// datagit_conflict in DESIGN.md §9.4.
type ConflictKind uint8

const (
	ConflictCell ConflictKind = iota + 1
	ConflictAddAdd
	ConflictDeleteModify
)

func (k ConflictKind) String() string {
	switch k {
	case ConflictCell:
		return "cell"
	case ConflictAddAdd:
		return "add_add"
	case ConflictDeleteModify:
		return "delete_modify"
	}
	return "?"
}

// Conflict is one unresolved disagreement. Col is meaningful only for
// ConflictCell; the other kinds are whole-row.
type Conflict struct {
	PK     PK
	Kind   ConflictKind
	Col    ColID
	HasCol bool
	Base   Value
	Ours   Value
	Theirs Value
}

func (c Conflict) String() string {
	if c.HasCol {
		return fmt.Sprintf("%s %s col=%d base=%s ours=%s theirs=%s",
			c.Kind, c.PK, c.Col, c.Base, c.Ours, c.Theirs)
	}
	return fmt.Sprintf("%s %s", c.Kind, c.PK)
}

// SortConflicts orders conflicts canonically so two implementations can be
// compared for exact equality.
func SortConflicts(cs []Conflict) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].PK != cs[j].PK {
			return cs[i].PK < cs[j].PK
		}
		if cs[i].Kind != cs[j].Kind {
			return cs[i].Kind < cs[j].Kind
		}
		return cs[i].Col < cs[j].Col
	})
}

// Table is a fully resolved table state: every live row, by key. Both the model
// and the implementation produce one of these, and the harness compares them.
type Table map[PK]Row

func (t Table) Equal(o Table) bool {
	if len(t) != len(o) {
		return false
	}
	for k, r := range t {
		or, ok := o[k]
		if !ok || !r.Equal(or) {
			return false
		}
	}
	return true
}

func (t Table) Clone() Table {
	if t == nil {
		return nil
	}
	c := make(Table, len(t))
	for k, r := range t {
		c[k] = r.Clone()
	}
	return c
}

func (t Table) Keys() []PK {
	out := make([]PK, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Schema is the minimal shape both sides need: the ordered value columns and
// which of them form the primary key.
type Schema struct {
	Cols   []ColID
	PKCols []ColID
}
