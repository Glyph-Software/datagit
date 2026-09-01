package core

import (
	"math/bits"
	"sort"
)

// ColMask is a bitmask over stable column ids (DESIGN.md §5.2, changed_cols).
//
// IMPORTANT — what a set bit does and does not mean:
//
// A set bit means "some write in this range touched this column". It does NOT
// mean the column's value differs from the base: a branch that changes a column
// and then changes it back leaves the bit set with the value equal to base.
//
// So the mask is a conservative SUPERSET of the genuinely-changed columns:
//
//	masks disjoint  => the sides changed no column in common  => merge clean
//	masks overlap   => the sides MAY have changed a column in common => compare values
//
// Overlap alone is never sufficient to declare a conflict. See merge.Merge and
// DESIGN.md §9.2.
type ColMask []uint64

func (m ColMask) Has(c ColID) bool {
	w := int(c / 64)
	if w >= len(m) {
		return false
	}
	return m[w]&(1<<(c%64)) != 0
}

func (m ColMask) Set(c ColID) ColMask {
	w := int(c / 64)
	for len(m) <= w {
		m = append(m, 0)
	}
	m[w] |= 1 << (c % 64)
	return m
}

// Or is the union, used to accumulate a mask across a range of commits.
func (m ColMask) Or(o ColMask) ColMask {
	n := len(m)
	if len(o) > n {
		n = len(o)
	}
	out := make(ColMask, n)
	for i := 0; i < n; i++ {
		var a, b uint64
		if i < len(m) {
			a = m[i]
		}
		if i < len(o) {
			b = o[i]
		}
		out[i] = a | b
	}
	return out
}

// Intersects reports whether the two masks share any column. Masks of different
// widths compare correctly: the shorter is treated as zero-extended, per
// DESIGN.md §10.5 (the mask only ever grows as columns are added).
func (m ColMask) Intersects(o ColMask) bool {
	n := len(m)
	if len(o) < n {
		n = len(o)
	}
	for i := 0; i < n; i++ {
		if m[i]&o[i] != 0 {
			return true
		}
	}
	return false
}

func (m ColMask) IsZero() bool {
	for _, w := range m {
		if w != 0 {
			return false
		}
	}
	return true
}

func (m ColMask) Count() int {
	n := 0
	for _, w := range m {
		n += bits.OnesCount64(w)
	}
	return n
}

// Cols lists the set columns in ascending order.
func (m ColMask) Cols() []ColID {
	var out []ColID
	for w, word := range m {
		for b := 0; b < 64; b++ {
			if word&(1<<b) != 0 {
				out = append(out, ColID(w*64+b))
			}
		}
	}
	return out
}

// UnionCols returns the ascending column ids set in either mask. This is the
// candidate set the merge algorithm examines; every column outside it is
// untouched by both sides and therefore equal to base on both.
func UnionCols(a, b ColMask) []ColID {
	seen := map[ColID]bool{}
	for _, c := range a.Cols() {
		seen[c] = true
	}
	for _, c := range b.Cols() {
		seen[c] = true
	}
	out := make([]ColID, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MaskOf returns the columns where two row images differ. Used when importing a
// write, to record what it actually changed.
func MaskOf(before, after Row, cols []ColID) ColMask {
	var m ColMask
	for _, c := range cols {
		if !before.Get(c).Equal(after.Get(c)) {
			m = m.Set(c)
		}
	}
	return m
}
