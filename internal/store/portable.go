package store

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// This file holds the small conversions that let one set of SQL statements run
// on both engines (§4.3).
//
// Two control-plane columns were PostgreSQL arrays -- a commit's parent ids and
// a table's primary-key column ids. MySQL has no array type, and the choices
// were a child table, a JSON column, or two schemas. JSON wins: both engines
// have it, both columns are read whole and never queried by element, and the
// alternative of a child table adds a join to the hottest control-plane read for
// a list that is almost always one element long.

// encodeDigests renders a commit's parents as a JSON array of hex strings.
//
// Hex rather than base64 so a human reading the control table sees the same
// digest text the CLI prints.
func encodeDigests(ds []hash.Digest) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = hex.EncodeToString(d[:])
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// decodeDigests parses what encodeDigests wrote.
func decodeDigests(raw []byte) ([]hash.Digest, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var hexes []string
	if err := json.Unmarshal(raw, &hexes); err != nil {
		return nil, fmt.Errorf("parent_ids is not a JSON array: %w", err)
	}
	out := make([]hash.Digest, 0, len(hexes))
	for _, h := range hexes {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != hash.Size {
			return nil, fmt.Errorf("parent_ids contains a malformed digest %q", h)
		}
		var d hash.Digest
		copy(d[:], b)
		out = append(out, d)
	}
	return out, nil
}

// encodeColIDs and decodeColIDs do the same for a primary-key column list.
func encodeColIDs(ids []core.ColID) string {
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = int(id)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func decodeColIDs(raw []byte) ([]core.ColID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var ns []int
	if err := json.Unmarshal(raw, &ns); err != nil {
		return nil, fmt.Errorf("pk_columns is not a JSON array: %w", err)
	}
	out := make([]core.ColID, len(ns))
	for i, n := range ns {
		out[i] = core.ColID(n)
	}
	return out, nil
}

// inList renders `col IN ($n, $n+1, ...)` with its arguments.
//
// It replaces PostgreSQL's `= ANY($1)`, which takes an array parameter MySQL has
// no equivalent for. An empty list renders a condition that is false rather than
// invalid SQL, because `IN ()` is a syntax error in both engines and silently
// dropping the clause would widen the query instead of narrowing it to nothing.
func inList(col string, vals []string, from int) (string, []any) {
	if len(vals) == 0 {
		return "1 = 0", nil
	}
	ph := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = "$" + strconv.Itoa(from+i)
		args[i] = v
	}
	return col + " IN (" + strings.Join(ph, ", ") + ")", args
}
