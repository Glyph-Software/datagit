package hash

import (
	"testing"
	"time"

	"github.com/Glyph-Software/datagit/internal/core"
)

// The golden values below pin `datagit.commit.v1` forever (PLAN.md M0.4, W3).
//
// If a change to internal/core or internal/hash makes one of these fail, that is
// not a test to update: it means the change would invalidate every commit hash
// ever written. Either revert it, or introduce `datagit.commit.v2` with an
// explicit migration story for existing history.

var testCols = []core.ColID{1, 2, 3, 4}

func fixedRow() core.Row {
	return core.Row{
		1: core.Text("sku-0001"),
		2: core.Int(42),
		3: core.MustNumeric("268.92"),
		4: core.Time(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)),
	}
}

func TestGoldenValueEncoding(t *testing.T) {
	cases := []struct {
		name string
		v    core.Value
		want string
	}{
		{"null", core.Null(), "00"},
		{"bool false", core.Bool_(false), "0100"},
		{"bool true", core.Bool_(true), "0101"},
		{"int 0", core.Int(0), "020000000000000000"},
		{"int 1", core.Int(1), "020000000000000001"},
		{"int -1", core.Int(-1), "02ffffffffffffffff"},
		{"float 1.5", core.Float(1.5), "033ff8000000000000"},
		{"float -0.0 collapses", core.Float(negZero()), "030000000000000000"},
		{"numeric 268.92", core.MustNumeric("268.92"), "04000000063236382e3932"},
		{"text abc", core.Text("abc"), "0500000003616263"},
		{"text empty", core.Text(""), "0500000000"},
		{"bytes", core.Bytes([]byte{0xde, 0xad}), "0600000002dead"},
		{"time epoch", core.Time(time.Unix(0, 0)), "070000000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.v.Encode(nil)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if hex(got) != tc.want {
				t.Errorf("FROZEN ENCODING CHANGED\n got %s\nwant %s", hex(got), tc.want)
			}
		})
	}
}

func TestGoldenCommitID(t *testing.T) {
	changes := []Change{{
		TableID: 7,
		PK:      core.MakePK(fixedRow(), []core.ColID{1}),
		Op:      core.OpUpdate,
		Changed: core.ColMask(nil).Set(3),
		Row:     fixedRow(),
	}}
	cd, err := ChangeDigest(changes, testCols)
	if err != nil {
		t.Fatalf("change digest: %v", err)
	}
	sd := SchemaDigest(7, []SchemaColumn{
		{ID: 1, Name: "sku", Type: "text", PK: true},
		{ID: 2, Name: "qty", Type: "int8", Nullable: true},
		{ID: 3, Name: "price", Type: "numeric(12,2)", Nullable: true},
		{ID: 4, Name: "updated_at", Type: "timestamptz", Nullable: true},
	})
	id := CommitID(CommitInput{
		RepoID:       [16]byte{1, 2, 3, 4},
		Parents:      []Digest{{0xaa}},
		ChangeDigest: cd,
		SchemaDigest: sd,
		Author:       "arun@example.com",
		AuthorAt:     time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		Message:      "Q4 outdoor price increase",
		ExternalRef:  "FIN-2291",
	})

	const want = "e0ebbb2bdf295ee1d8fb155b02eca0bfba9c94069ba9f3162b21f060c17a459b"
	if id.String() != want {
		t.Errorf("FROZEN COMMIT HASH CHANGED\n got %s\nwant %s\n\n"+
			"This is not a test to update. Changing it invalidates every commit id\n"+
			"ever written. Revert, or introduce datagit.commit.v2 with a migration.",
			id, want)
	}
}

// TestParentOrderIrrelevant: a merge commit's id must not depend on which parent
// was recorded first.
func TestParentOrderIrrelevant(t *testing.T) {
	a, b := Digest{0x01}, Digest{0x02}
	in := CommitInput{Author: "x", Message: "m"}
	in.Parents = []Digest{a, b}
	first := CommitID(in)
	in.Parents = []Digest{b, a}
	if second := CommitID(in); first != second {
		t.Errorf("parent order changed the commit id: %s vs %s", first, second)
	}
}

// TestChangeOrderIrrelevant: two clients making the same change set must produce
// the same commit, whatever order they send the rows in.
func TestChangeOrderIrrelevant(t *testing.T) {
	mk := func(pk string) Change {
		r := core.Row{1: core.Text(pk), 2: core.Int(1)}
		return Change{TableID: 1, PK: core.MakePK(r, []core.ColID{1}), Op: core.OpUpdate, Row: r}
	}
	forward := []Change{mk("a"), mk("b"), mk("c")}
	reverse := []Change{mk("c"), mk("b"), mk("a")}
	d1, err := ChangeDigest(forward, []core.ColID{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := ChangeDigest(reverse, []core.ColID{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("change order changed the digest: %s vs %s", d1, d2)
	}
}

// TestMaskWidthIrrelevant: DESIGN.md §10.5 lets the mask grow as columns are
// added. A commit must not change its hash because a later schema widened it.
func TestMaskWidthIrrelevant(t *testing.T) {
	narrow := core.ColMask{0b1010}
	wide := core.ColMask{0b1010, 0, 0}
	r := core.Row{1: core.Text("k")}
	pk := core.MakePK(r, []core.ColID{1})
	d1, _ := LeafDigest(Change{TableID: 1, PK: pk, Op: core.OpUpdate, Changed: narrow, Row: r}, []core.ColID{1})
	d2, _ := LeafDigest(Change{TableID: 1, PK: pk, Op: core.OpUpdate, Changed: wide, Row: r}, []core.ColID{1})
	if d1 != d2 {
		t.Errorf("mask width changed the digest: %s vs %s", d1, d2)
	}
}

// TestNumericNormalization: values the database considers equal must hash
// equally, or the same logical state would produce different commit ids.
func TestNumericNormalization(t *testing.T) {
	same := [][]string{
		{"1.1", "1.10", "+1.1", "01.100"},
		{"0", "-0", "0.0", "0.000", "+0"},
		{"-5", "-5.00", "-05"},
	}
	for _, group := range same {
		var first []byte
		for i, s := range group {
			v := core.MustNumeric(s)
			enc, err := v.Encode(nil)
			if err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			if i == 0 {
				first = enc
				continue
			}
			if hex(enc) != hex(first) {
				t.Errorf("%q and %q are the same number but encode differently: %s vs %s",
					group[0], s, hex(first), hex(enc))
			}
		}
	}
}

// TestLengthPrefixingPreventsAmbiguity: without length prefixes, adjacent text
// fields could be re-split, so two different rows would hash the same.
func TestLengthPrefixingPreventsAmbiguity(t *testing.T) {
	cols := []core.ColID{1, 2}
	pkCols := []core.ColID{1}
	r1 := core.Row{1: core.Text("ab"), 2: core.Text("c")}
	r2 := core.Row{1: core.Text("a"), 2: core.Text("bc")}
	d1, _ := LeafDigest(Change{TableID: 1, PK: core.MakePK(r1, pkCols), Op: core.OpUpdate, Row: r1}, cols)
	d2, _ := LeafDigest(Change{TableID: 1, PK: core.MakePK(r2, pkCols), Op: core.OpUpdate, Row: r2}, cols)
	if d1 == d2 {
		t.Error("distinct rows produced the same digest: framing is ambiguous")
	}
}

// TestMerkleOddNodePromoted guards the CVE-2012-2459 shape, where duplicating a
// lone odd node lets two different trees produce the same root.
func TestMerkleOddNodePromoted(t *testing.T) {
	a, b, c := Digest{1}, Digest{2}, Digest{3}
	three := MerkleRoot([]Digest{a, b, c})
	// If the odd node were duplicated, this four-leaf set would collide with it.
	four := MerkleRoot([]Digest{a, b, c, c})
	if three == four {
		t.Error("odd node is being duplicated, not promoted: trees collide")
	}
}

func TestNaNRefused(t *testing.T) {
	v := core.Value{Kind: core.KindFloat, Float: nan()}
	if _, err := v.Encode(nil); err == nil {
		t.Error("NaN should be refused: it has many bit patterns and no canonical form")
	}
}

func TestEmptyChangeSetIsStable(t *testing.T) {
	d1, _ := ChangeDigest(nil, testCols)
	d2, _ := ChangeDigest([]Change{}, testCols)
	if d1 != d2 || d1.IsZero() {
		t.Errorf("empty change set must have a stable non-zero digest, got %s and %s", d1, d2)
	}
}

func hex(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&0xf])
	}
	return string(out)
}

func negZero() float64 { z := 0.0; return -z }
func nan() float64     { z := 0.0; return z / z }
