// Package store is DataGit's M1 foundation: repository and table tracking, the
// atomic commit write path, time travel, history, blame, and diff, against a
// real database.
//
// M1 covers the default branch only. Branching, sessions, and merge arrive in
// M2 and M3; the types here already carry the branch and chain fields those
// milestones need, because Phase 0 finding F1 established that the chain has to
// be stored from the first commit rather than retrofitted.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
	"github.com/Glyph-Software/datagit/internal/pg"
)

// DefaultBranch is the branch whose live table is the materialization.
const DefaultBranch = "main"

type Store struct {
	pool *pg.Pool
	ad   adapter.Adapter
}

func New(pool *pg.Pool, ad adapter.Adapter) *Store { return &Store{pool: pool, ad: ad} }

// Repo identifies a repository.
type Repo struct {
	ID            uuid.UUID
	Name          string
	DefaultBranch uuid.UUID
}

// Table is a tracked table plus its stable column ids.
type Table struct {
	ID        int64
	RepoID    uuid.UUID
	Physical  string
	Mode      adapter.Mode
	State     string
	Columns   []adapter.Column
	PKColumns []core.ColID
}

// Spec renders the table for the adapter.
func (t *Table) Spec() *adapter.TableSpec {
	return &adapter.TableSpec{
		ID: uint64(t.ID), PhysicalName: t.Physical, Mode: t.Mode,
		Columns: t.Columns, PKColumns: t.PKColumns,
	}
}

func (t *Table) ColIDs() []core.ColID {
	out := make([]core.ColID, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, c.ID)
	}
	return out
}

func (t *Table) Column(id core.ColID) (adapter.Column, bool) {
	for _, c := range t.Columns {
		if c.ID == id {
			return c, true
		}
	}
	return adapter.Column{}, false
}

// --- Bootstrapping (M1.1, §17.2) ---

// InitControlSchema creates the control tables and records the schema version.
// Idempotent, so it is safe to run on every startup.
func (s *Store) InitControlSchema(ctx context.Context) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if err := tx.Exec(ctx, catalog.ControlSchema); err != nil {
			return fmt.Errorf("control schema: %w", err)
		}
		return tx.Exec(ctx,
			`INSERT INTO datagit_meta (key, value) VALUES ('control_schema_version', $1)
			 ON CONFLICT (key) DO NOTHING`,
			fmt.Sprint(catalog.ControlSchemaVersion))
	})
}

// CheckControlSchema refuses to run against a newer control schema than this
// build understands (§17.2).
func (s *Store) CheckControlSchema(ctx context.Context) error {
	var v int
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT value::int FROM datagit_meta WHERE key = 'control_schema_version'`).Scan(&v)
	if err != nil {
		return fmt.Errorf("control schema version unreadable (run repo init?): %w", err)
	}
	if v > catalog.ControlSchemaVersion {
		return fmt.Errorf(
			"control schema is version %d but this build understands %d: upgrade DataGit",
			v, catalog.ControlSchemaVersion)
	}
	return nil
}

// CreateRepo registers a repository and its default branch with a root commit.
func (s *Store) CreateRepo(ctx context.Context, name, principal string) (*Repo, error) {
	repo := &Repo{ID: uuid.New(), Name: name, DefaultBranch: uuid.New()}
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		now, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		if err := tx.Exec(ctx,
			`INSERT INTO datagit_repo (id, name, default_branch) VALUES ($1,$2,$3)`,
			repo.ID, name, repo.DefaultBranch); err != nil {
			return fmt.Errorf("create repo: %w", err)
		}

		// The root commit. Its chain is the single default-branch segment at seq
		// 0; every later commit records its own (finding F1).
		chain := []adapter.Segment{{BranchID: repo.DefaultBranch, Seq: 0}}
		rootID := hash.CommitID(hash.CommitInput{
			RepoID:       repo.ID,
			ChangeDigest: mustEmptyDigest(),
			Author:       principal,
			AuthorAt:     now,
			Message:      "repository created",
		})
		if err := tx.Exec(ctx,
			`INSERT INTO datagit_ref (id, repo_id, kind, name, head_commit, head_seq, chain, protected, created_by)
			 VALUES ($1,$2,'branch',$3,$4,0,$5,true,$6)`,
			repo.DefaultBranch, repo.ID, DefaultBranch, rootID[:], mustJSON(chain), principal); err != nil {
			return fmt.Errorf("create default branch: %w", err)
		}
		return insertCommit(ctx, tx, repo.ID, repo.DefaultBranch, 0, rootID, nil,
			principal, now, "repository created", "", mustEmptyDigest(), hash.Digest{}, chain)
	})
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func (s *Store) LookupRepo(ctx context.Context, name string) (*Repo, error) {
	r := &Repo{Name: name}
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT id, default_branch FROM datagit_repo WHERE name = $1`, name).
		Scan(&r.ID, &r.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("no repository %q: %w", name, err)
	}
	return r, nil
}

// --- Tracking (M1.2) ---

// Track brings a live table under version control.
//
// Refusals are deliberate and named, per DESIGN.md §3.2 and §10.5 rule 5: a
// table with no stable primary key cannot be `versioned`, and a column whose
// type has no canonical encoding makes the table ineligible. Approximating
// either would produce history that cannot be reproduced or hashed.
func (s *Store) Track(ctx context.Context, repo *Repo, physical string, mode adapter.Mode) (*Table, error) {
	var t *Table
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		cols, pk, err := introspect(ctx, tx, physical)
		if err != nil {
			return err
		}
		if len(pk) == 0 {
			return fmt.Errorf(
				"table %q has no primary key: %s mode requires one, because a row's "+
					"primary key is its identity for all of history (§3.2)", physical, mode)
		}
		var unsupported []string
		for _, c := range cols {
			if _, ok := postgres.KindFor(c.SQLType); !ok {
				unsupported = append(unsupported, fmt.Sprintf("%s (%s)", c.Name, c.SQLType))
			}
		}
		if len(unsupported) > 0 && mode == adapter.ModeVersioned {
			return fmt.Errorf(
				"table %q cannot be tracked in versioned mode: no canonical encoding for %s. "+
					"Approximating would make commit hashes unreproducible (§10.5 rule 5)",
				physical, strings.Join(unsupported, ", "))
		}

		t = &Table{RepoID: repo.ID, Physical: physical, Mode: mode, State: "backfilling",
			Columns: cols, PKColumns: pk}

		pkInts := make([]int32, len(pk))
		for i, p := range pk {
			pkInts[i] = int32(p)
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO datagit_table (repo_id, physical_name, mode, pk_columns, state)
			 VALUES ($1,$2,$3,$4,'backfilling') RETURNING id`,
			repo.ID, physical, string(mode), pkInts).Scan(&t.ID); err != nil {
			return fmt.Errorf("register table: %w", err)
		}
		for i, c := range t.Columns {
			if err := tx.Exec(ctx,
				`INSERT INTO datagit_column (table_id, id, name, sql_type, kind, nullable, is_pk, ordinal)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				t.ID, int32(c.ID), c.Name, c.SQLType, int16(c.Kind), c.Nullable,
				containsCol(pk, c.ID), i); err != nil {
				return fmt.Errorf("register column %s: %w", c.Name, err)
			}
		}
		return s.ad.CreateSidecar(ctx, tx, t.Spec())
	})
	if err != nil {
		return nil, err
	}
	if err := s.backfill(ctx, repo, t); err != nil {
		return nil, err
	}
	return t, nil
}

// backfill copies the live table into the sidecar as the root import (M1.3,
// §6.4). History before this point does not exist and is never fabricated: the
// root version is honestly labelled an import.
func (s *Store) backfill(ctx context.Context, repo *Repo, t *Table) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		var head []byte
		var headSeq int64
		if err := tx.QueryRow(ctx,
			`SELECT head_commit, head_seq FROM datagit_ref WHERE id = $1`,
			repo.DefaultBranch).Scan(&head, &headSeq); err != nil {
			return err
		}

		src := make([]string, 0, len(t.Columns))
		dst := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			src = append(src, quote(c.Name))
			dst = append(dst, quote(catalog.SidecarColumn(uint32(c.ID))))
		}
		// Chunking and rate limiting belong here for large tables; the single
		// statement is correct and adequate at M1 scale.
		sql := fmt.Sprintf(
			`INSERT INTO %s (branch_id, seq_from, seq_to, op, commit_id, changed_cols, %s)
			 SELECT $1, $2, %d, %d, $3, $4, %s FROM %s
			 ON CONFLICT DO NOTHING`,
			quote(catalog.SidecarTable(t.Physical)), strings.Join(dst, ", "),
			postgres.MaxSeq, core.OpInsert, strings.Join(src, ", "), quote(t.Physical))
		if err := tx.Exec(ctx, sql, repo.DefaultBranch, headSeq, head, []byte{}); err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
		return tx.Exec(ctx, `UPDATE datagit_table SET state = 'active' WHERE id = $1`, t.ID)
	})
}

func (s *Store) LoadTable(ctx context.Context, repo *Repo, physical string) (*Table, error) {
	t := &Table{RepoID: repo.ID, Physical: physical}
	tx := s.pool.Direct()
	var pkInts []int32
	var mode string
	if err := tx.QueryRow(ctx,
		`SELECT id, mode, pk_columns, state FROM datagit_table WHERE repo_id=$1 AND physical_name=$2`,
		repo.ID, physical).Scan(&t.ID, &mode, &pkInts, &t.State); err != nil {
		return nil, fmt.Errorf("table %q is not tracked: %w", physical, err)
	}
	t.Mode = adapter.Mode(mode)
	for _, p := range pkInts {
		t.PKColumns = append(t.PKColumns, core.ColID(p))
	}
	rows, err := tx.Query(ctx,
		`SELECT id, name, sql_type, kind, nullable FROM datagit_column
		  WHERE table_id=$1 AND dropped_at IS NULL ORDER BY ordinal`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c adapter.Column
		var id int32
		var kind int16
		if err := rows.Scan(&id, &c.Name, &c.SQLType, &kind, &c.Nullable); err != nil {
			return nil, err
		}
		c.ID, c.Kind = core.ColID(id), core.Kind(kind)
		t.Columns = append(t.Columns, c)
	}
	return t, rows.Err()
}

// --- The write path (M1.4, §6.1) ---

// Change is one row's worth of a commit.
type Change struct {
	PK  core.PK
	Op  core.Op
	Row core.Row
}

// CommitRequest is one atomic commit. DESIGN.md §6.1: a commit is a single RPC
// carrying its whole change set. There is no staging on the default branch.
type CommitRequest struct {
	Repo         *Repo
	Table        *Table
	Branch       string
	Changes      []Change
	Author       string // the authenticated principal, never client-supplied (§15.2)
	Message      string
	ExternalRef  string
	ExpectedHead *hash.Digest // optimistic concurrency; nil to skip

	// ExtraParents adds parents beyond the branch head, for merge commits.
	// They are folded into the commit HASH, not merely recorded afterwards:
	// otherwise recomputing the chain fails on every merge, and
	// `verify --integrity` would be useless exactly where history is most
	// complex.
	ExtraParents []hash.Digest
}

type CommitResult struct {
	ID      hash.Digest
	Seq     int64
	Changed int
}

// Commit applies the whole change set in one transaction: live-table writes,
// version records, and the commit record land together or not at all.
func (s *Store) Commit(ctx context.Context, req CommitRequest) (*CommitResult, error) {
	if req.Author == "" {
		return nil, fmt.Errorf("commit requires an authenticated author (§15.2)")
	}
	res := &CommitResult{}
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		branchID, headCommit, headSeq, chain, err := s.loadRef(ctx, tx, req.Repo, req.Branch)
		if err != nil {
			return err
		}

		// The ref lock serializes seq assignment (§11.3). Phase 0 finding F10:
		// this caps a branch at ~850 commits/s regardless of writer count, which
		// is why audit-mode tables skip it.
		if req.Table.Mode == adapter.ModeVersioned {
			if err := s.ad.AcquireRefLock(ctx, tx, branchID); err != nil {
				return fmt.Errorf("ref lock: %w", err)
			}
			// Re-read under the lock: the head may have moved while we queued.
			if _, headCommit, headSeq, chain, err = s.loadRefLocked(ctx, tx, req.Repo, req.Branch); err != nil {
				return err
			}
		}
		if req.ExpectedHead != nil && *req.ExpectedHead != headCommit {
			return fmt.Errorf("branch %q moved: expected head %s, actual %s",
				req.Branch, req.ExpectedHead.Short(), headCommit.Short())
		}

		now, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		newSeq := headSeq + 1
		isDefault := branchID == req.Repo.DefaultBranch

		// Sort by primary key so two concurrent commits touching overlapping keys
		// take row locks in the same order and cannot deadlock (§6.1 property 3).
		changes := append([]Change(nil), req.Changes...)
		sort.Slice(changes, func(i, j int) bool { return changes[i].PK < changes[j].PK })

		var leaves []hash.Change
		for _, ch := range changes {
			before, live, err := s.currentRow(ctx, tx, req.Table, chain, ch.PK)
			if err != nil {
				return err
			}
			switch ch.Op {
			case core.OpDelete:
				if !live {
					continue // deleting an absent row is a no-op, not a tombstone
				}
			default:
				if live && before.Equal(ch.Row) {
					continue // no-op write: nothing changed, so nothing to record
				}
			}

			op := ch.Op
			if op != core.OpDelete {
				op = core.OpUpdate
				if !live {
					op = core.OpInsert
				}
			}
			mask := core.MaskOf(before, ch.Row, req.Table.ColIDs())

			if isDefault {
				if err := s.applyLive(ctx, tx, req.Table, ch, op, live); err != nil {
					return err
				}
			}
			if err := s.closeOpen(ctx, tx, req.Table, branchID, ch.PK, newSeq); err != nil {
				return err
			}
			if err := s.insertVersion(ctx, tx, req.Table, branchID, newSeq, op, ch, mask); err != nil {
				return err
			}
			leaves = append(leaves, hash.Change{
				TableID: uint64(req.Table.ID), PK: ch.PK, Op: op, Changed: mask, Row: ch.Row,
			})
			res.Changed++
		}

		cd, err := hash.ChangeDigest(leaves, req.Table.ColIDs())
		if err != nil {
			return err
		}
		sd := schemaDigest(req.Table)
		parents := append([]hash.Digest{headCommit}, req.ExtraParents...)
		id := hash.CommitID(hash.CommitInput{
			RepoID: req.Repo.ID, Parents: parents,
			ChangeDigest: cd, SchemaDigest: sd,
			Author: req.Author, AuthorAt: now,
			Message: req.Message, ExternalRef: req.ExternalRef,
		})

		// Stamp the versions with the real commit id.
		if err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE %s SET commit_id = $1 WHERE branch_id = $2 AND seq_from = $3`,
			quote(catalog.SidecarTable(req.Table.Physical))), id[:], branchID, newSeq); err != nil {
			return fmt.Errorf("stamp versions: %w", err)
		}

		newChain := append([]adapter.Segment{{BranchID: branchID, Seq: newSeq}}, chain[1:]...)
		if err := insertCommit(ctx, tx, req.Repo.ID, branchID, newSeq, id,
			parents, req.Author, now, req.Message, req.ExternalRef,
			cd, sd, newChain); err != nil {
			return err
		}
		if err := tx.Exec(ctx,
			`UPDATE datagit_ref SET head_commit=$1, head_seq=$2, chain=$3 WHERE id=$4`,
			id[:], newSeq, mustJSON(newChain), branchID); err != nil {
			return fmt.Errorf("advance ref: %w", err)
		}
		res.ID, res.Seq = id, newSeq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Store) applyLive(ctx context.Context, tx adapter.Tx, t *Table, ch Change, op core.Op, live bool) error {
	pkVals, err := decodePK(ch.PK, t)
	if err != nil {
		return err
	}
	if op == core.OpDelete {
		where, args := pkWhere(t, pkVals, 1)
		return tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s`, quote(t.Physical), where), args...)
	}
	cols := make([]string, 0, len(t.Columns))
	ph := make([]string, 0, len(t.Columns))
	args := make([]any, 0, len(t.Columns))
	for i, c := range t.Columns {
		cols = append(cols, quote(c.Name))
		ph = append(ph, fmt.Sprintf("$%d", i+1))
		v, err := bind(ch.Row.Get(c.ID))
		if err != nil {
			return err
		}
		args = append(args, v)
	}
	pkNames := make([]string, 0, len(t.PKColumns))
	for _, id := range t.PKColumns {
		c, _ := t.Column(id)
		pkNames = append(pkNames, quote(c.Name))
	}
	sets := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		if containsCol(t.PKColumns, c.ID) {
			continue
		}
		sets = append(sets, fmt.Sprintf("%s = EXCLUDED.%s", quote(c.Name), quote(c.Name)))
	}
	sql := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s`,
		quote(t.Physical), strings.Join(cols, ", "), strings.Join(ph, ", "),
		strings.Join(pkNames, ", "), strings.Join(sets, ", "))
	return tx.Exec(ctx, sql, args...)
}

func (s *Store) closeOpen(ctx context.Context, tx adapter.Tx, t *Table, branch uuid.UUID, pk core.PK, at int64) error {
	pkVals, err := decodePK(pk, t)
	if err != nil {
		return err
	}
	where, args := sidecarPKWhere(t, pkVals, 3)
	args = append([]any{at, branch}, args...)
	return tx.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET seq_to = $1 WHERE branch_id = $2 AND session_id IS NULL
		   AND seq_to = %d AND %s`,
		quote(catalog.SidecarTable(t.Physical)), postgres.MaxSeq, where), args...)
}

func (s *Store) insertVersion(ctx context.Context, tx adapter.Tx, t *Table,
	branch uuid.UUID, seq int64, op core.Op, ch Change, mask core.ColMask) error {
	cols := []string{"branch_id", "seq_from", "seq_to", "op", "commit_id", "changed_cols"}
	args := []any{branch, seq, postgres.MaxSeq, int16(op), []byte{}, maskBytes(mask)}
	for _, c := range t.Columns {
		cols = append(cols, catalog.SidecarColumn(uint32(c.ID)))
		var v any
		var err error
		if op == core.OpDelete && !containsCol(t.PKColumns, c.ID) {
			v = nil // a tombstone carries its key and nothing else
		} else if op == core.OpDelete {
			v, err = bind(pkValueOf(ch, t, c.ID))
		} else {
			v, err = bind(ch.Row.Get(c.ID))
		}
		if err != nil {
			return err
		}
		args = append(args, v)
	}
	q := make([]string, len(cols))
	qn := make([]string, len(cols))
	for i := range cols {
		q[i] = quote(cols[i])
		qn[i] = fmt.Sprintf("$%d", i+1)
	}
	return tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
		quote(catalog.SidecarTable(t.Physical)), strings.Join(q, ", "), strings.Join(qn, ", ")), args...)
}

func pkValueOf(ch Change, t *Table, id core.ColID) core.Value {
	if ch.Row != nil {
		return ch.Row.Get(id)
	}
	vals, err := decodePK(ch.PK, t)
	if err != nil {
		return core.Null()
	}
	for i, p := range t.PKColumns {
		if p == id && i < len(vals) {
			return vals[i]
		}
	}
	return core.Null()
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func containsCol(s []core.ColID, c core.ColID) bool {
	for _, x := range s {
		if x == c {
			return true
		}
	}
	return false
}

func maskBytes(m core.ColMask) []byte {
	out := make([]byte, 0, len(m)*8)
	for _, w := range m {
		out = core.AppendUint64(out, w)
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func mustEmptyDigest() hash.Digest {
	d, _ := hash.ChangeDigest(nil, nil)
	return d
}

func schemaDigest(t *Table) hash.Digest {
	cols := make([]hash.SchemaColumn, 0, len(t.Columns))
	for _, c := range t.Columns {
		cols = append(cols, hash.SchemaColumn{
			ID: c.ID, Name: c.Name, Type: c.SQLType,
			Nullable: c.Nullable, PK: containsCol(t.PKColumns, c.ID),
		})
	}
	return hash.SchemaDigest(uint64(t.ID), cols)
}

func insertCommit(ctx context.Context, tx adapter.Tx, repo, branch uuid.UUID, seq int64,
	id hash.Digest, parents []hash.Digest, author string, at time.Time,
	msg, ref string, cd, sd hash.Digest, chain []adapter.Segment) error {
	ps := make([][]byte, 0, len(parents))
	for _, p := range parents {
		pp := p
		ps = append(ps, pp[:])
	}
	return tx.Exec(ctx,
		`INSERT INTO datagit_commit
		   (id, repo_id, branch_id, seq, parent_ids, author, author_at, committer,
		    committed_at, message, external_ref, change_digest, schema_digest, chain)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$6,$7,$8,$9,$10,$11,$12)`,
		id[:], repo, branch, seq, ps, author, at, msg, ref, cd[:], sd[:], mustJSON(chain))
}
