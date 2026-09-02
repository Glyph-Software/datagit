package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// Revert creates a NEW commit that undoes a prior one (M1.8, §16.1).
//
// Nothing is erased and no history is rewritten. That is the difference between
// a revert and a rollback: a point-in-time restore would lose every legitimate
// write since, and rewriting history would break the hash chain and the audit
// trail it exists to support.
//
// A revert can conflict with later work — if a row the target commit changed has
// since changed again, undoing it would silently discard the newer value. Rather
// than guess, those keys are reported and the revert is refused unless `force`
// is set, which matches DESIGN.md's rule that ambiguity is surfaced, never
// resolved automatically.
func (s *Store) Revert(ctx context.Context, repo *Repo, t *Table, branch string,
	target hash.Digest, author, message string, force bool) (*CommitResult, error) {

	tx := s.pool.Direct()
	var seq int64
	var parents [][]byte
	if err := tx.QueryRow(ctx,
		`SELECT seq, parent_ids FROM datagit_commit WHERE id = $1`, target[:]).
		Scan(&seq, &parents); err != nil {
		return nil, fmt.Errorf("unknown commit %s: %w", target.Short(), err)
	}
	if seq == 0 {
		return nil, fmt.Errorf("cannot revert the root commit")
	}

	// What the target commit did.
	entries, err := s.Diff(ctx, repo, t, branch, seq-1, seq)
	if err != nil {
		return nil, fmt.Errorf("revert: reading the target commit: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("commit %s changed nothing to revert", target.Short())
	}

	// What the branch looks like now.
	_, _, headSeq, _, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return nil, err
	}

	var changed []string
	var undo []Change
	for _, e := range entries {
		if headSeq > seq {
			// Has this key moved since the target commit?
			later, err := s.Diff(ctx, repo, t, branch, seq, headSeq)
			if err != nil {
				return nil, err
			}
			for _, l := range later {
				if l.PK == e.PK {
					changed = append(changed, core.PKString(l.After, t.PKColumns))
				}
			}
		}
		switch e.Op {
		case core.OpInsert:
			undo = append(undo, Change{PK: e.PK, Op: core.OpDelete})
		case core.OpDelete:
			undo = append(undo, Change{PK: e.PK, Op: core.OpInsert, Row: e.Before})
		default:
			undo = append(undo, Change{PK: e.PK, Op: core.OpUpdate, Row: e.Before})
		}
	}

	if len(changed) > 0 && !force {
		return nil, fmt.Errorf(
			"revert of %s would discard later changes to %d key(s): %s. "+
				"Re-run with force to proceed, or revert the later commits first",
			target.Short(), len(changed), strings.Join(dedupe(changed), ", "))
	}

	if message == "" {
		message = fmt.Sprintf("Revert %s", target.Short())
	}
	return s.Commit(ctx, CommitRequest{
		Repo: repo, Table: t, Branch: branch, Changes: undo,
		Author: author, Message: message, ExternalRef: "revert:" + target.String(),
	})
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// --- Export (M1.12, §17.5) ---

type exportHeader struct {
	Kind       string    `json:"kind"`
	Repo       string    `json:"repo"`
	Table      string    `json:"table"`
	Mode       string    `json:"mode"`
	Encoding   string    `json:"encoding"`
	ExportedAt time.Time `json:"exported_at"`
}

type exportColumn struct {
	Kind     string `json:"kind"`
	ID       uint32 `json:"id"`
	Name     string `json:"name"`
	SQLType  string `json:"sql_type"`
	Nullable bool   `json:"nullable"`
	PK       bool   `json:"pk"`
}

type exportCommit struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	Seq         int64     `json:"seq"`
	Parents     []string  `json:"parents"`
	Author      string    `json:"author"`
	CommittedAt time.Time `json:"committed_at"`
	Message     string    `json:"message"`
	ExternalRef string    `json:"external_ref,omitempty"`
	Integrity   string    `json:"integrity"`
}

type exportVersion struct {
	Kind    string            `json:"kind"`
	Commit  string            `json:"commit"`
	SeqFrom int64             `json:"seq_from"`
	SeqTo   int64             `json:"seq_to"`
	Op      string            `json:"op"`
	Values  map[string]string `json:"values"`
}

// Export writes a table's full history as newline-delimited JSON (§17.5).
//
// Adoption must not be a one-way door. This is what makes leaving cheap: the
// history can be archived, audited offline, or re-imported, and untracking then
// leaves the live table exactly as it was.
func (s *Store) Export(ctx context.Context, repo *Repo, t *Table, branch string, w io.Writer) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(exportHeader{
		Kind: "header", Repo: repo.Name, Table: t.Physical, Mode: string(t.Mode),
		Encoding: core.CanonicalVersion, ExportedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	for _, c := range t.Columns {
		if err := enc.Encode(exportColumn{
			Kind: "column", ID: uint32(c.ID), Name: c.Name, SQLType: c.SQLType,
			Nullable: c.Nullable, PK: containsCol(t.PKColumns, c.ID),
		}); err != nil {
			return err
		}
	}

	tx := s.pool.Direct()
	branchID, _, _, _, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return err
	}

	commits, err := tx.Query(ctx,
		`SELECT id, seq, parent_ids, author, committed_at, message, external_ref, integrity
		   FROM datagit_commit WHERE repo_id=$1 AND branch_id=$2 ORDER BY seq`,
		repo.ID, branchID)
	if err != nil {
		return err
	}
	for commits.Next() {
		var id []byte
		var parents [][]byte
		var e exportCommit
		var at time.Time
		if err := commits.Scan(&id, &e.Seq, &parents, &e.Author, &at,
			&e.Message, &e.ExternalRef, &e.Integrity); err != nil {
			commits.Close()
			return err
		}
		e.Kind, e.ID, e.CommittedAt = "commit", hexs(id), at
		for _, p := range parents {
			e.Parents = append(e.Parents, hexs(p))
		}
		if err := enc.Encode(e); err != nil {
			commits.Close()
			return err
		}
	}
	commits.Close()
	if err := commits.Err(); err != nil {
		return err
	}

	sel := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		sel = append(sel, quote(catalog.SidecarColumn(uint32(c.ID))))
	}
	vers, err := tx.Query(ctx, fmt.Sprintf(
		`SELECT %s, op, seq_from, seq_to, commit_id FROM %s
		  WHERE branch_id=$1 AND session_id IS NULL ORDER BY seq_from, %s`,
		strings.Join(sel, ", "), quote(catalog.SidecarTable(t.Physical)),
		strings.Join(sel[:len(t.PKColumns)], ", ")), branchID)
	if err != nil {
		return err
	}
	defer vers.Close()
	for vers.Next() {
		vals := make([]any, len(t.Columns))
		dest := make([]any, 0, len(t.Columns)+4)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var op int16
		var seqFrom, seqTo int64
		var cid []byte
		dest = append(dest, &op, &seqFrom, &seqTo, &cid)
		if err := vers.Scan(dest...); err != nil {
			return err
		}
		row := valuesToRow(vals, t)
		out := exportVersion{
			Kind: "version", Commit: hexs(cid), SeqFrom: seqFrom, SeqTo: seqTo,
			Op: core.Op(op).String(), Values: map[string]string{},
		}
		for _, c := range t.Columns {
			out.Values[c.Name] = row.Get(c.ID).String()
		}
		if err := enc.Encode(out); err != nil {
			return err
		}
	}
	return vers.Err()
}

func hexs(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&0xf])
	}
	return string(out)
}

var _ = adapter.Eq
