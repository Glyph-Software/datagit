// Package core holds the value and change types shared by the reference model
// (internal/model) and the real implementation (internal/engine, internal/hash).
//
// Only *data* lives here. No resolution, diff, or merge logic: the whole point of
// the differential harness is that the model and the implementation share no
// algorithm, so anything algorithmic in this package would weaken the evidence.
package core

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ColID is a stable column identifier. DESIGN.md §10.5 rule 1: sidecar columns
// are named by immutable id, never by name, so renames are metadata-only and a
// drop-then-re-add yields a new id rather than colliding with old history.
type ColID uint32

// Kind is a value's type tag.
//
// FROZEN. These numbers are part of the canonical encoding (§12.1) and appear in
// every commit hash ever written. Never renumber; only append.
type Kind uint8

const (
	KindNull    Kind = 0
	KindBool    Kind = 1
	KindInt     Kind = 2 // int64
	KindFloat   Kind = 3 // IEEE-754 binary64
	KindNumeric Kind = 4 // exact decimal, held as a normalized string
	KindText    Kind = 5 // UTF-8
	KindBytes   Kind = 6
	KindTime    Kind = 7 // microseconds since the Unix epoch, UTC
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindNumeric:
		return "numeric"
	case KindText:
		return "text"
	case KindBytes:
		return "bytes"
	case KindTime:
		return "time"
	}
	return "?"
}

// Value is one cell.
type Value struct {
	Kind  Kind
	Bool  bool
	Int   int64  // KindInt; also microseconds for KindTime
	Float float64
	Text  string // KindText, and the normalized decimal for KindNumeric
	Bytes []byte
}

func Null() Value         { return Value{Kind: KindNull} }
func Bool_(b bool) Value  { return Value{Kind: KindBool, Bool: b} }
func Int(i int64) Value   { return Value{Kind: KindInt, Int: i} }
func Text(s string) Value { return Value{Kind: KindText, Text: s} }
func Bytes(b []byte) Value {
	c := make([]byte, len(b))
	copy(c, b)
	return Value{Kind: KindBytes, Bytes: c}
}

// Float builds a float value. NaN is rejected by Encode, not here, so that the
// error surfaces at the write path with the column named.
func Float(f float64) Value {
	if f == 0 {
		f = 0 // collapse -0.0 to +0.0 so equal values encode identically
	}
	return Value{Kind: KindFloat, Float: f}
}

// Numeric holds an exact decimal. The input is normalized so that any two
// spellings of the same number encode identically — "1.10", "1.1", and "+1.1"
// all become "1.1". This matters: the database considers them equal, so the
// canonical encoding must too, or the same logical state would hash differently.
func Numeric(s string) (Value, error) {
	n, err := normalizeDecimal(s)
	if err != nil {
		return Value{}, err
	}
	return Value{Kind: KindNumeric, Text: n}, nil
}

// MustNumeric is Numeric for literals known to be well-formed.
func MustNumeric(s string) Value {
	v, err := Numeric(s)
	if err != nil {
		panic(err)
	}
	return v
}

// Time holds an instant at microsecond precision — the resolution both
// PostgreSQL `timestamptz` and MySQL `DATETIME(6)` store. Sub-microsecond input
// is truncated, not rounded, so encoding is idempotent.
func Time(t time.Time) Value {
	return Value{Kind: KindTime, Int: t.UTC().UnixMicro()}
}

func (v Value) IsNull() bool { return v.Kind == KindNull }

func (v Value) AsTime() time.Time { return time.UnixMicro(v.Int).UTC() }

// normalizeDecimal produces the canonical spelling of a decimal number:
// optional leading '-', no leading zeros except a single "0" before the point,
// no trailing zeros after the point, no point if the fraction is empty, and
// "0" for every representation of zero.
func normalizeDecimal(s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", fmt.Errorf("core: empty numeric")
	}
	neg := false
	switch t[0] {
	case '+':
		t = t[1:]
	case '-':
		neg = true
		t = t[1:]
	}
	intPart, fracPart := t, ""
	if i := strings.IndexByte(t, '.'); i >= 0 {
		intPart, fracPart = t[:i], t[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return "", fmt.Errorf("core: malformed numeric %q", s)
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("core: malformed numeric %q", s)
		}
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	fracPart = strings.TrimRight(fracPart, "0")

	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if out == "0" {
		return "0", nil // -0 and 0 are the same number
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}

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
	case KindBool:
		return v.Bool == o.Bool
	case KindInt, KindTime:
		return v.Int == o.Int
	case KindFloat:
		// NaN != NaN is SQL's rule for `=`, but DataGit needs an equivalence
		// relation for merge, and Encode refuses NaN anyway.
		return v.Float == o.Float
	case KindNumeric, KindText:
		return v.Text == o.Text
	case KindBytes:
		return string(v.Bytes) == string(o.Bytes)
	}
	return false
}

func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "NULL"
	case KindBool:
		return strconv.FormatBool(v.Bool)
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case KindNumeric:
		return v.Text
	case KindText:
		return strconv.Quote(v.Text)
	case KindBytes:
		return fmt.Sprintf("0x%x", v.Bytes)
	case KindTime:
		return v.AsTime().Format(time.RFC3339Nano)
	}
	return "?"
}

// CanonicalVersion identifies the frozen encoding. It appears in every commit
// hash. Changing the encoding requires a new version tag and a migration story
// for existing history (PLAN.md W3).
const CanonicalVersion = "datagit.commit.v1"

// Encode appends the canonical byte encoding of v to dst.
//
// FROZEN as of `datagit.commit.v1`. Every byte of this function is part of the
// commit hash, so changing it invalidates every commit id ever written. Guarded
// by golden tests in internal/hash.
//
// The encoding is tagged and length-prefixed so that no two distinct values can
// produce the same bytes: without a length prefix, Text("ab")+Text("c") and
// Text("a")+Text("bc") would be indistinguishable in a concatenation.
//
// It also preserves value equality: two values the database considers equal
// encode identically. That is why numerics are normalized and -0.0 is collapsed.
func (v Value) Encode(dst []byte) ([]byte, error) {
	dst = append(dst, byte(v.Kind))
	switch v.Kind {
	case KindNull:
		return dst, nil
	case KindBool:
		if v.Bool {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	case KindInt, KindTime:
		var b [8]byte
		// Big-endian two's complement: fixed width, so no length prefix needed.
		binary.BigEndian.PutUint64(b[:], uint64(v.Int))
		return append(dst, b[:]...), nil
	case KindFloat:
		if math.IsNaN(v.Float) {
			// NaN has many bit patterns and no total order. Refusing it here is
			// better than silently hashing one representation, and better than
			// discovering the ambiguity after history exists.
			return nil, fmt.Errorf("core: NaN cannot be canonically encoded")
		}
		f := v.Float
		if f == 0 {
			f = 0 // -0.0 and +0.0 are the same number
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
		return append(dst, b[:]...), nil
	case KindNumeric, KindText:
		return appendLenPrefixed(dst, []byte(v.Text)), nil
	case KindBytes:
		return appendLenPrefixed(dst, v.Bytes), nil
	}
	return nil, fmt.Errorf("core: cannot encode kind %d", v.Kind)
}

// MustEncode is Encode for callers that have already validated their values.
func (v Value) MustEncode(dst []byte) []byte {
	out, err := v.Encode(dst)
	if err != nil {
		panic(err)
	}
	return out
}

// appendLenPrefixed writes a big-endian uint32 length followed by the payload.
func appendLenPrefixed(dst, payload []byte) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(payload)))
	dst = append(dst, b[:]...)
	return append(dst, payload...)
}

// AppendLenPrefixed exposes the framing so callers outside this package encode
// their own fields the same way.
func AppendLenPrefixed(dst, payload []byte) []byte { return appendLenPrefixed(dst, payload) }

// AppendUint32 appends a fixed-width big-endian uint32.
func AppendUint32(dst []byte, n uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	return append(dst, b[:]...)
}

// AppendUint64 appends a fixed-width big-endian uint64.
func AppendUint64(dst []byte, n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return append(dst, b[:]...)
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

// Encode appends the canonical encoding of the row, restricted to `cols` and in
// their given order.
//
// The column set is supplied rather than taken from the map so that the encoding
// depends on the *schema*, not on which columns happen to be present in this
// image. A row missing a column encodes it as NULL, which is what Get returns.
func (r Row) Encode(dst []byte, cols []ColID) ([]byte, error) {
	dst = AppendUint32(dst, uint32(len(cols)))
	for _, c := range cols {
		dst = AppendUint32(dst, uint32(c))
		var err error
		if dst, err = r.Get(c).Encode(dst); err != nil {
			return nil, fmt.Errorf("column %d: %w", c, err)
		}
	}
	return dst, nil
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
	buf := make([]byte, 0, 32)
	buf = AppendUint32(buf, uint32(len(pkCols)))
	for _, c := range pkCols {
		buf = r.Get(c).MustEncode(buf)
	}
	return PK(buf)
}

// PKString renders a primary key for human-readable output. The canonical form
// is binary, so it is not printable as-is.
func PKString(r Row, pkCols []ColID) string {
	parts := make([]string, 0, len(pkCols))
	for _, c := range pkCols {
		parts = append(parts, r.Get(c).String())
	}
	return strings.Join(parts, "|")
}
