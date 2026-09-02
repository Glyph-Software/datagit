package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Glyph-Software/datagit/internal/adapter"

	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
)

// bind converts a canonical value into a driver argument.
func bind(v core.Value) (any, error) {
	switch v.Kind {
	case core.KindNull:
		return nil, nil
	case core.KindBool:
		return v.Bool, nil
	case core.KindInt:
		return v.Int, nil
	case core.KindFloat:
		return v.Float, nil
	case core.KindNumeric:
		var n pgtype.Numeric
		if err := n.Scan(v.Text); err != nil {
			return nil, fmt.Errorf("numeric %q: %w", v.Text, err)
		}
		return n, nil
	case core.KindText:
		return v.Text, nil
	case core.KindBytes:
		return v.Bytes, nil
	case core.KindTime:
		return v.AsTime(), nil
	}
	return nil, fmt.Errorf("cannot bind kind %s", v.Kind)
}

// fromDriver converts a scanned database value into a canonical value.
//
// This is the inverse of bind, and the two must agree exactly: a value written
// and read back has to encode identically, or a row would appear to change
// merely by round-tripping and every commit hash over it would differ.
func fromDriver(x any, kind core.Kind) (core.Value, error) {
	if x == nil {
		return core.Null(), nil
	}
	switch val := x.(type) {
	case bool:
		return core.Bool_(val), nil
	case int16:
		return core.Int(int64(val)), nil
	case int32:
		return core.Int(int64(val)), nil
	case int64:
		return core.Int(val), nil
	case float32:
		return core.Float(float64(val)), nil
	case float64:
		return core.Float(val), nil
	case string:
		return fromText(val, kind)
	case []byte:
		// MySQL hands back []byte for a great many column types -- DECIMAL,
		// VARCHAR, TEXT, and the temporal types when time parsing is off -- so the
		// bytes alone do not say what the value is. The COLUMN'S DECLARED KIND
		// does, and it is authoritative: reading a DECIMAL as opaque bytes would
		// put the wrong tag in the canonical encoding and change the commit hash
		// for a value that did not change (§12.1).
		if kind == core.KindBytes {
			return core.Bytes(val), nil
		}
		return fromText(string(val), kind)
	case time.Time:
		return core.Time(val), nil
	case pgtype.Numeric:
		s, err := numericString(val)
		if err != nil {
			return core.Value{}, err
		}
		return core.Numeric(s)
	case [16]byte: // uuid
		return core.Text(uuidText(val)), nil
	}
	return core.Value{}, fmt.Errorf("cannot convert %T to a canonical value", x)
}

// fromText interprets a textual driver value as the column's declared kind.
func fromText(s string, kind core.Kind) (core.Value, error) {
	switch kind {
	case core.KindNumeric:
		return core.Numeric(s)
	case core.KindInt:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return core.Value{}, fmt.Errorf("column declared integer holds %q: %w", s, err)
		}
		return core.Int(n), nil
	case core.KindFloat:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return core.Value{}, fmt.Errorf("column declared float holds %q: %w", s, err)
		}
		return core.Float(f), nil
	case core.KindBool:
		switch s {
		case "1", "true", "TRUE", "t", "y", "yes":
			return core.Bool_(true), nil
		case "0", "false", "FALSE", "f", "n", "no":
			return core.Bool_(false), nil
		}
		return core.Value{}, fmt.Errorf("column declared boolean holds %q", s)
	case core.KindTime:
		for _, layout := range []string{
			time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02",
		} {
			if ts, err := time.Parse(layout, s); err == nil {
				return core.Time(ts.UTC()), nil
			}
		}
		return core.Value{}, fmt.Errorf("column declared timestamp holds %q", s)
	case core.KindBytes:
		return core.Bytes([]byte(s)), nil
	default:
		return core.Text(s), nil
	}
}

func numericString(n pgtype.Numeric) (string, error) {
	if !n.Valid {
		return "0", nil
	}
	if n.NaN {
		return "", fmt.Errorf("numeric NaN has no canonical encoding")
	}
	// Render exactly: unscaled integer shifted by the exponent, never via float.
	i := new(big.Int).Set(n.Int)
	neg := i.Sign() < 0
	if neg {
		i.Neg(i)
	}
	digits := i.String()
	exp := int(n.Exp)
	var out string
	switch {
	case exp >= 0:
		out = digits + strings.Repeat("0", exp)
	default:
		p := -exp
		for len(digits) <= p {
			digits = "0" + digits
		}
		out = digits[:len(digits)-p] + "." + digits[len(digits)-p:]
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}

func uuidText(u [16]byte) string {
	const hexd = "0123456789abcdef"
	buf := make([]byte, 0, 36)
	for i, b := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf = append(buf, '-')
		}
		buf = append(buf, hexd[b>>4], hexd[b&0xf])
	}
	return string(buf)
}

func valuesToRow(vals []any, t *Table) core.Row {
	r := make(core.Row, len(t.Columns))
	for i, c := range t.Columns {
		if i >= len(vals) {
			break
		}
		v, err := fromDriver(vals[i], c.Kind)
		if err != nil {
			v = core.Null()
		}
		r[c.ID] = v
	}
	return r
}

func scanRow(rows adapter.Rows, t *Table) (core.Row, core.Op, error) {
	vals := make([]any, len(t.Columns))
	dest := make([]any, 0, len(t.Columns)+1)
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	var op int16
	dest = append(dest, &op)
	if err := rows.Scan(dest...); err != nil {
		return nil, 0, err
	}
	return valuesToRow(vals, t), core.Op(op), nil
}

// decodePK reverses core.MakePK back into the individual key values, so the
// binary canonical key can be turned into SQL predicates.
func decodePK(pk core.PK, t *Table) ([]core.Value, error) {
	b := []byte(pk)
	if len(b) < 4 {
		return nil, fmt.Errorf("malformed primary key")
	}
	n := binary.BigEndian.Uint32(b[:4])
	b = b[4:]
	out := make([]core.Value, 0, n)
	for i := uint32(0); i < n; i++ {
		v, rest, err := decodeValue(b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		b = rest
	}
	if len(out) != len(t.PKColumns) {
		return nil, fmt.Errorf("primary key has %d parts, table has %d", len(out), len(t.PKColumns))
	}
	return out, nil
}

func decodeValue(b []byte) (core.Value, []byte, error) {
	if len(b) == 0 {
		return core.Value{}, nil, fmt.Errorf("truncated value")
	}
	kind := core.Kind(b[0])
	b = b[1:]
	switch kind {
	case core.KindNull:
		return core.Null(), b, nil
	case core.KindBool:
		if len(b) < 1 {
			return core.Value{}, nil, fmt.Errorf("truncated bool")
		}
		return core.Bool_(b[0] == 1), b[1:], nil
	case core.KindInt, core.KindTime:
		if len(b) < 8 {
			return core.Value{}, nil, fmt.Errorf("truncated int")
		}
		n := int64(binary.BigEndian.Uint64(b[:8]))
		if kind == core.KindTime {
			return core.Value{Kind: core.KindTime, Int: n}, b[8:], nil
		}
		return core.Int(n), b[8:], nil
	case core.KindFloat:
		if len(b) < 8 {
			return core.Value{}, nil, fmt.Errorf("truncated float")
		}
		bits := binary.BigEndian.Uint64(b[:8])
		return core.Value{Kind: core.KindFloat, Float: float64FromBits(bits)}, b[8:], nil
	case core.KindNumeric, core.KindText, core.KindBytes:
		if len(b) < 4 {
			return core.Value{}, nil, fmt.Errorf("truncated length")
		}
		n := binary.BigEndian.Uint32(b[:4])
		b = b[4:]
		if uint32(len(b)) < n {
			return core.Value{}, nil, fmt.Errorf("truncated payload")
		}
		payload, rest := b[:n], b[n:]
		switch kind {
		case core.KindText:
			return core.Text(string(payload)), rest, nil
		case core.KindNumeric:
			return core.Value{Kind: core.KindNumeric, Text: string(payload)}, rest, nil
		default:
			return core.Bytes(payload), rest, nil
		}
	}
	return core.Value{}, nil, fmt.Errorf("unknown kind %d in primary key", kind)
}

func float64FromBits(b uint64) float64 { return math.Float64frombits(b) }

// pkWhere builds a predicate over the LIVE table's primary key columns.
func pkWhere(t *Table, vals []core.Value, start int) (string, []any) {
	var parts []string
	var args []any
	for i, id := range t.PKColumns {
		c, _ := t.Column(id)
		parts = append(parts, fmt.Sprintf("%s = $%d", quote(c.Name), start+i))
		v, _ := bind(vals[i])
		args = append(args, v)
	}
	return strings.Join(parts, " AND "), args
}

// sidecarPKWhere builds the same predicate over the SIDECAR's mirrored columns.
func sidecarPKWhere(t *Table, vals []core.Value, start int) (string, []any) {
	var parts []string
	var args []any
	for i, id := range t.PKColumns {
		parts = append(parts, fmt.Sprintf("%s = $%d",
			quote(catalog.SidecarColumn(uint32(id))), start+i))
		v, _ := bind(vals[i])
		args = append(args, v)
	}
	return strings.Join(parts, " AND "), args
}

var _ = context.Background
