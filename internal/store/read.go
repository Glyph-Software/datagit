package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// loadRef reads a branch's head and its stored resolution chain.
//
// The chain is READ, never rebuilt from parent refs (Phase 0 finding F1).
func (s *Store) loadRef(ctx context.Context, tx adapter.Tx, repo *Repo, name string) (
	uuid.UUID, hash.Digest, int64, []adapter.Segment, error) {
	return s.refRow(ctx, tx, repo, name, "")
}

func (s *Store) loadRefLocked(ctx context.Context, tx adapter.Tx, repo *Repo, name string) (
	uuid.UUID, hash.Digest, int64, []adapter.Segment, error) {
	return s.refRow(ctx, tx, repo, name, " FOR UPDATE")
}

func (s *Store) refRow(ctx context.Context, tx adapter.Tx, repo *Repo, name, suffix string) (
	uuid.UUID, hash.Digest, int64, []adapter.Segment, error) {
	var id uuid.UUID
	var head []byte
	var seq int64
	var chainJSON []byte
	err := tx.QueryRow(ctx,
		`SELECT id, head_commit, head_seq, chain FROM datagit_ref
		  WHERE repo_id=$1 AND kind='branch' AND name=$2`+suffix,
		repo.ID, name).Scan(&id, &head, &seq, &chainJSON)
	if err != nil {
		return uuid.Nil, hash.Digest{}, 0, nil, fmt.Errorf("no branch %q: %w", name, err)
	}
	var d hash.Digest
	copy(d[:], head)
	var chain []adapter.Segment
	if err := json.Unmarshal(chainJSON, &chain); err != nil {
		return uuid.Nil, hash.Digest{}, 0, nil, fmt.Errorf("branch %q chain: %w", name, err)
	}
	if len(chain) == 0 {
		chain = []adapter.Segment{{BranchID: id, Seq: seq}}
	}
	return id, d, seq, chain, nil
}

// chainAt returns the chain recorded on a commit, so historical reads resolve
// against the world as it was rather than as it became (finding F1).
func (s *Store) chainAt(ctx context.Context, tx adapter.Tx, commit hash.Digest) ([]adapter.Segment, error) {
	var b []byte
	if err := tx.QueryRow(ctx, `SELECT chain FROM datagit_commit WHERE id=$1`, commit[:]).Scan(&b); err != nil {
		return nil, fmt.Errorf("unknown commit %s: %w", commit.Short(), err)
	}
	var chain []adapter.Segment
	return chain, json.Unmarshal(b, &chain)
}

// keyFilter builds a primary-key predicate for a canonical key.
//
// Safe to push into the resolution arms because row identity is immutable
// (finding F6). See adapter.ResolveSpec.KeyFilter.
func keyFilter(t *Table, pk core.PK) (adapter.Expr, error) {
	vals, err := decodePK(pk, t)
	if err != nil {
		return nil, err
	}
	terms := make([]adapter.Expr, 0, len(t.PKColumns))
	for i, id := range t.PKColumns {
		terms = append(terms, adapter.Compare{Col: id, Op: adapter.Eq, Value: vals[i]})
	}
	if len(terms) == 1 {
		return terms[0], nil
	}
	return adapter.And{Terms: terms}, nil
}

// currentRow resolves one key through a whole chain.
//
// It must walk the FULL chain, not just the branch's own segment: a branch that
// has never written a key still inherits it, and deleting such a row would
// otherwise look like a no-op on an absent key and be silently skipped.
func (s *Store) currentRow(ctx context.Context, tx adapter.Tx, t *Table,
	chain []adapter.Segment, pk core.PK) (core.Row, bool, error) {
	kf, err := keyFilter(t, pk)
	if err != nil {
		return nil, false, err
	}
	q, err := s.ad.ResolveQuery(&adapter.ResolveSpec{
		Table: t.Spec(), Chain: chain, KeyFilter: kf,
	})
	if err != nil {
		return nil, false, err
	}
	rows, err := tx.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, false, fmt.Errorf("resolve key: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	row, _, err := scanRow(rows, t)
	if err != nil {
		return nil, false, err
	}
	// The resolve query already filters tombstones in the outer scope, so a row
	// reaching here is live by construction.
	return row, true, nil
}

// ReadOptions selects what to read.
type ReadOptions struct {
	Branch string
	At     *hash.Digest // a specific commit; nil means the branch head
	AsOf   *time.Time   // a timestamp; resolved to the latest commit at or before it
	Filter adapter.Expr
	Limit  int
	After  core.PK
}

// Read resolves a table (M1.6, §7.2/§7.3).
func (s *Store) Read(ctx context.Context, repo *Repo, t *Table, opt ReadOptions) ([]core.Row, error) {
	tx := s.pool.Direct()
	branch := opt.Branch
	if branch == "" {
		branch = DefaultBranch
	}
	branchID, head, headSeq, chain, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return nil, err
	}

	switch {
	case opt.At != nil:
		if chain, err = s.chainAt(ctx, tx, *opt.At); err != nil {
			return nil, err
		}
	case opt.AsOf != nil:
		// Time as an address (§7.2). committed_at comes from the database clock
		// inside the ref-locked commit transaction, so it is monotonic per branch
		// regardless of replica clock skew.
		var b []byte
		err := tx.QueryRow(ctx,
			`SELECT id FROM datagit_commit
			  WHERE repo_id=$1 AND branch_id=$2 AND committed_at <= $3
			  ORDER BY seq DESC LIMIT 1`,
			repo.ID, branchID, opt.AsOf.UTC()).Scan(&b)
		if err != nil {
			return nil, fmt.Errorf("no commit on %q at or before %s: %w",
				branch, opt.AsOf.Format(time.RFC3339), err)
		}
		var d hash.Digest
		copy(d[:], b)
		if chain, err = s.chainAt(ctx, tx, d); err != nil {
			return nil, err
		}
	default:
		_ = head
		_ = headSeq
	}

	q, err := s.ad.ResolveQuery(&adapter.ResolveSpec{
		Table: t.Spec(), Chain: chain, Filter: opt.Filter,
		Limit: opt.Limit, After: opt.After,
	})
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	defer rows.Close()

	var out []core.Row
	for rows.Next() {
		r, _, err := scanRow(rows, t)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VersionRecord is one row of a key's history.
type VersionRecord struct {
	SeqFrom  int64
	SeqTo    int64
	Op       core.Op
	CommitID hash.Digest
	Changed  core.ColMask
	Row      core.Row
	Author   string
	At       time.Time
	Message  string
}

// History returns the version chain for one key (M1.6).
func (s *Store) History(ctx context.Context, repo *Repo, t *Table, branch string, pk core.PK) ([]VersionRecord, error) {
	tx := s.pool.Direct()
	branchID, _, _, _, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return nil, err
	}
	pkVals, err := decodePK(pk, t)
	if err != nil {
		return nil, err
	}
	where, args := sidecarPKWhere(t, pkVals, 2)
	args = append([]any{branchID}, args...)

	sel := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		sel = append(sel, "v."+quote(catalog.SidecarColumn(uint32(c.ID))))
	}
	sql := fmt.Sprintf(
		`SELECT %s, v.op, v.seq_from, v.seq_to, v.commit_id, v.changed_cols,
		        c.author, c.committed_at, c.message
		   FROM %s v LEFT JOIN datagit_commit c ON c.id = v.commit_id
		  WHERE v.branch_id=$1 AND v.session_id IS NULL AND %s
		  ORDER BY v.seq_from DESC`,
		strings.Join(sel, ", "), quote(catalog.SidecarTable(t.Physical)), aliasPK(where, "v"))

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	defer rows.Close()

	var out []VersionRecord
	for rows.Next() {
		dest := make([]any, 0, len(t.Columns)+9)
		vals := make([]any, len(t.Columns))
		for i := range t.Columns {
			dest = append(dest, &vals[i])
		}
		var rec VersionRecord
		var op int16
		var cid, mask []byte
		var author, msg *string
		var at *time.Time
		dest = append(dest, &op, &rec.SeqFrom, &rec.SeqTo, &cid, &mask, &author, &at, &msg)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		rec.Op = core.Op(op)
		copy(rec.CommitID[:], cid)
		rec.Changed = bytesToMask(mask)
		rec.Row = valuesToRow(vals, t)
		if author != nil {
			rec.Author = *author
		}
		if msg != nil {
			rec.Message = *msg
		}
		if at != nil {
			rec.At = *at
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CellBlame attributes one cell's current value.
type CellBlame struct {
	Col      core.ColID
	Value    core.Value
	CommitID hash.Digest
	Author   string
	At       time.Time
	Message  string
}

// Blame walks a key's version chain, attributing each column to the most recent
// version whose changed_cols marks it (M1.6).
//
// The mask is a superset (finding F2), so a bit can be set with the value equal
// to its predecessor. Blame therefore confirms by value: it reports the version
// where the value last actually changed, not merely where a bit was set.
func (s *Store) Blame(ctx context.Context, repo *Repo, t *Table, branch string, pk core.PK, cols []core.ColID) ([]CellBlame, error) {
	hist, err := s.History(ctx, repo, t, branch, pk)
	if err != nil {
		return nil, err
	}
	if len(hist) == 0 {
		return nil, fmt.Errorf("no history for key")
	}
	if len(cols) == 0 {
		cols = t.ColIDs()
	}
	current := hist[0].Row
	out := make([]CellBlame, 0, len(cols))
	for _, c := range cols {
		b := CellBlame{Col: c, Value: current.Get(c)}
		// hist is newest-first. Walk forward until the value differs; the last
		// version that still held it is where it was introduced.
		attributed := hist[0]
		for _, v := range hist {
			if v.Op == core.OpDelete || !v.Row.Get(c).Equal(current.Get(c)) {
				break
			}
			attributed = v
		}
		b.CommitID, b.Author, b.At, b.Message =
			attributed.CommitID, attributed.Author, attributed.At, attributed.Message
		out = append(out, b)
	}
	return out, nil
}

// DiffEntry is one changed row between two commits.
type DiffEntry struct {
	PK      core.PK
	Op      core.Op
	Before  core.Row
	After   core.Row
	Changed core.ColMask
}

// Diff is the two-point interval scan (M1.7, §8.1): only versions whose
// boundaries fall in the range are visited, so cost is proportional to the
// change, not the table.
func (s *Store) Diff(ctx context.Context, repo *Repo, t *Table, branch string, fromSeq, toSeq int64) ([]DiffEntry, error) {
	tx := s.pool.Direct()
	branchID, _, headSeq, chain, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return nil, err
	}
	if toSeq < 0 {
		toSeq = headSeq
	}

	q, err := s.ad.DiffQuery(t.Spec(), branchID, fromSeq, toSeq)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	touched := map[core.PK]bool{}
	for rows.Next() {
		dest := make([]any, 0, len(t.Columns)+2)
		vals := make([]any, len(t.Columns))
		for i := range t.Columns {
			dest = append(dest, &vals[i])
		}
		var op int16
		var seqFrom int64
		dest = append(dest, &op, &seqFrom)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		touched[core.MakePK(valuesToRow(vals, t), t.PKColumns)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Resolve each touched key at both endpoints. The keys are the change set, so
	// this stays proportional to the change.
	beforeChain := replaceHead(chain, branchID, fromSeq)
	afterChain := replaceHead(chain, branchID, toSeq)
	var out []DiffEntry
	for pk := range touched {
		before, hadBefore, err := s.currentRowChain(ctx, tx, t, beforeChain, pk)
		if err != nil {
			return nil, err
		}
		after, hasAfter, err := s.currentRowChain(ctx, tx, t, afterChain, pk)
		if err != nil {
			return nil, err
		}
		switch {
		case hadBefore && !hasAfter:
			out = append(out, DiffEntry{PK: pk, Op: core.OpDelete, Before: before})
		case !hadBefore && hasAfter:
			out = append(out, DiffEntry{PK: pk, Op: core.OpInsert, After: after,
				Changed: core.MaskOf(nil, after, t.ColIDs())})
		case hadBefore && hasAfter && !before.Equal(after):
			out = append(out, DiffEntry{PK: pk, Op: core.OpUpdate, Before: before, After: after,
				Changed: core.MaskOf(before, after, t.ColIDs())})
		}
	}
	return out, nil
}

func replaceHead(chain []adapter.Segment, branch uuid.UUID, seq int64) []adapter.Segment {
	out := append([]adapter.Segment(nil), chain...)
	if len(out) > 0 && out[0].BranchID == branch {
		out[0].Seq = seq
	}
	return out
}

func (s *Store) currentRowChain(ctx context.Context, tx adapter.Tx, t *Table,
	chain []adapter.Segment, pk core.PK) (core.Row, bool, error) {
	if len(chain) == 0 {
		return nil, false, nil
	}
	return s.currentRow(ctx, tx, t, chain, pk)
}

// VerifyDrift compares the live table against the resolved default branch
// (M1.11, §6.3). Out-of-band writes are possible in `open` mode and this is what
// detects them.
type DriftReport struct {
	LiveOnly    int
	VersionOnly int
	Mismatched  int
}

func (s *Store) VerifyDrift(ctx context.Context, repo *Repo, t *Table) (*DriftReport, error) {
	live, err := s.scanLive(ctx, t)
	if err != nil {
		return nil, err
	}
	resolved, err := s.Read(ctx, repo, t, ReadOptions{Branch: DefaultBranch})
	if err != nil {
		return nil, err
	}
	rmap := make(map[core.PK]core.Row, len(resolved))
	for _, r := range resolved {
		rmap[core.MakePK(r, t.PKColumns)] = r
	}
	rep := &DriftReport{}
	for _, lr := range live {
		pk := core.MakePK(lr, t.PKColumns)
		rr, ok := rmap[pk]
		if !ok {
			rep.LiveOnly++
			continue
		}
		if !lr.Equal(rr) {
			rep.Mismatched++
		}
		delete(rmap, pk)
	}
	rep.VersionOnly = len(rmap)
	return rep, nil
}

func (s *Store) scanLive(ctx context.Context, t *Table) ([]core.Row, error) {
	sel := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		sel = append(sel, quote(c.Name))
	}
	rows, err := s.pool.Direct().Query(ctx,
		fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(sel, ", "), quote(t.Physical)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Row
	for rows.Next() {
		vals := make([]any, len(t.Columns))
		dest := make([]any, len(t.Columns))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, valuesToRow(vals, t))
	}
	return out, rows.Err()
}

// VerifyIntegrity recomputes the commit hash chain (M1.11, §17.3).
//
// A commit marked `integrity = 'purged'` is expected not to match: a hard purge
// removes rows and records the discontinuity rather than re-hashing to hide it
// (§13.4). Those are skipped, not silently accepted.
func (s *Store) VerifyIntegrity(ctx context.Context, repo *Repo, branch string) error {
	tx := s.pool.Direct()
	branchID, _, _, _, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, parent_ids, author, author_at, message, external_ref,
		        change_digest, schema_digest, integrity, seq
		   FROM datagit_commit WHERE repo_id=$1 AND branch_id=$2 ORDER BY seq`,
		repo.ID, branchID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, cd, sd []byte
		var parents [][]byte
		var author, msg, ref, integrity string
		var at time.Time
		var seq int64
		if err := rows.Scan(&id, &parents, &author, &at, &msg, &ref, &cd, &sd, &integrity, &seq); err != nil {
			return err
		}
		if integrity == "purged" {
			continue
		}
		in := hash.CommitInput{RepoID: repo.ID, Author: author, AuthorAt: at,
			Message: msg, ExternalRef: ref}
		copy(in.ChangeDigest[:], cd)
		copy(in.SchemaDigest[:], sd)
		for _, p := range parents {
			var d hash.Digest
			copy(d[:], p)
			in.Parents = append(in.Parents, d)
		}
		var stored hash.Digest
		copy(stored[:], id)
		if got := hash.CommitID(in); got != stored {
			return fmt.Errorf(
				"integrity: commit at seq %d has id %s but its content hashes to %s",
				seq, stored.Short(), got.Short())
		}
	}
	return rows.Err()
}

// Untrack removes a table from version control (M1.12, §17.5).
//
// The live table is untouched at every step, because it was never modified. An
// application that stops calling DataGit is fully functional the moment it does;
// what it loses is history going forward, not data.
func (s *Store) Untrack(ctx context.Context, repo *Repo, t *Table) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`,
			quote(catalog.SidecarTable(t.Physical)))); err != nil {
			return fmt.Errorf("drop sidecar: %w", err)
		}
		return tx.Exec(ctx, `DELETE FROM datagit_table WHERE id=$1`, t.ID)
	})
}

// aliasPK prefixes the mirrored primary-key columns in a predicate with a table
// alias, for queries that join the sidecar against datagit_commit.
func aliasPK(where, alias string) string {
	return strings.ReplaceAll(where, `"c_`, alias+`."c_`)
}

func bytesToMask(b []byte) core.ColMask {
	var m core.ColMask
	for i := 0; i+8 <= len(b); i += 8 {
		var w uint64
		for j := 0; j < 8; j++ {
			w = w<<8 | uint64(b[i+j])
		}
		m = append(m, w)
	}
	return m
}

var _ = postgres.MaxSeq

// ListTables returns every tracked table in a repository.
func (s *Store) ListTables(ctx context.Context, repo *Repo) ([]*Table, error) {
	rows, err := s.pool.Direct().Query(ctx,
		`SELECT physical_name FROM datagit_table WHERE repo_id=$1 ORDER BY physical_name`, repo.ID)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*Table, 0, len(names))
	for _, n := range names {
		t, err := s.LoadTable(ctx, repo, n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// CommitRecord is one entry of the commit log.
type CommitRecord struct {
	ID          hash.Digest
	Seq         int64
	Author      string
	CommittedAt time.Time
	Message     string
	ExternalRef string
	Integrity   string
}

// Log returns a branch's commits, newest first.
func (s *Store) Log(ctx context.Context, repo *Repo, branch string, limit int) ([]CommitRecord, error) {
	tx := s.pool.Direct()
	branchID, _, _, _, err := s.loadRef(ctx, tx, repo, branch)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := tx.Query(ctx,
		`SELECT id, seq, author, committed_at, message, external_ref, integrity
		   FROM datagit_commit WHERE repo_id=$1 AND branch_id=$2
		  ORDER BY seq DESC LIMIT $3`, repo.ID, branchID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var r CommitRecord
		var id []byte
		if err := rows.Scan(&id, &r.Seq, &r.Author, &r.CommittedAt,
			&r.Message, &r.ExternalRef, &r.Integrity); err != nil {
			return nil, err
		}
		copy(r.ID[:], id)
		out = append(out, r)
	}
	return out, rows.Err()
}
