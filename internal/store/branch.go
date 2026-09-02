package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// MaxChainDepth caps resolution segments (§18). Branch creation past it is
// refused rather than allowed to degrade reads unboundedly.
const MaxChainDepth = 8

// Ref is a branch or a tag.
type Ref struct {
	ID         uuid.UUID
	Kind       string
	Name       string
	Head       hash.Digest
	HeadSeq    int64
	Parent     string
	ForkCommit hash.Digest
	ForkSeq    int64
	Chain      []adapter.Segment
	Protected  bool
}

// CreateBranch forks a branch. O(1): a fork point and a captured chain, no data
// copied (§G4).
//
// The chain is CAPTURED here, not derived later (Phase 0 finding F1). A
// descendant that rebuilt its chain from its ancestors' live fork points would
// silently inherit rows it never asked for, the moment any ancestor absorbed its
// own parent.
func (s *Store) CreateBranch(ctx context.Context, repo *Repo, name, from, principal string) (*Ref, error) {
	var ref *Ref
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		parentID, parentHead, parentSeq, parentChain, err := s.loadRef(ctx, tx, repo, from)
		if err != nil {
			return err
		}
		// A stored chain ALWAYS includes the branch itself at index 0, followed by
		// its inherited tail. Every reader and writer assumes that shape:
		// resolution walks it as-is, and a commit replaces index 0 with the new
		// seq while keeping the tail.
		//
		// The child's tail is the parent pinned at the parent's CURRENT head,
		// plus whatever the parent itself inherited. Pinning is the point
		// (finding F1): if the parent later absorbs its own parent, this branch
		// must not move.
		id := uuid.New()
		chain := make([]adapter.Segment, 0, len(parentChain)+1)
		chain = append(chain, adapter.Segment{BranchID: id, Seq: 0})
		chain = append(chain, adapter.Segment{BranchID: parentID, Seq: parentSeq})
		chain = append(chain, parentChain[1:]...)

		if len(chain) > MaxChainDepth {
			return fmt.Errorf(
				"branch %q would have a resolution chain of depth %d, over the cap of %d: "+
					"merge the chain down before forking again (§18)",
				name, len(chain), MaxChainDepth)
		}

		ref = &Ref{
			ID: id, Kind: "branch", Name: name, Head: parentHead, HeadSeq: 0,
			Parent: from, ForkCommit: parentHead, ForkSeq: parentSeq, Chain: chain,
		}
		return tx.Exec(ctx,
			`INSERT INTO datagit_ref
			   (id, repo_id, kind, name, head_commit, head_seq, parent_ref, fork_commit, fork_seq, chain, created_by)
			 VALUES ($1,$2,'branch',$3,$4,0,$5,$4,$6,$7,$8)`,
			id, repo.ID, name, parentHead[:], parentID, parentSeq, mustJSON(chain), principal)
	})
	if err != nil {
		return nil, err
	}
	return ref, nil
}

// CreateTag pins a commit under a human name (§3.1).
func (s *Store) CreateTag(ctx context.Context, repo *Repo, name string, at hash.Digest, principal string) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		chain, err := s.chainAt(ctx, tx, at)
		if err != nil {
			return err
		}
		return tx.Exec(ctx,
			`INSERT INTO datagit_ref (id, repo_id, kind, name, head_commit, chain, created_by)
			 VALUES ($1,$2,'tag',$3,$4,$5,$6)`,
			uuid.New(), repo.ID, name, at[:], mustJSON(chain), principal)
	})
}

// ListRefs returns every branch and tag.
func (s *Store) ListRefs(ctx context.Context, repo *Repo) ([]Ref, error) {
	rows, err := s.pool.Direct().Query(ctx,
		`SELECT r.id, r.kind, r.name, r.head_commit, r.head_seq,
		        coalesce(p.name,''), r.fork_commit,
		        coalesce(r.fork_seq,0), r.chain, r.protected
		   FROM datagit_ref r LEFT JOIN datagit_ref p ON p.id = r.parent_ref
		  WHERE r.repo_id = $1 ORDER BY r.kind, r.name`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		var head, fork, chainJSON []byte
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &head, &r.HeadSeq,
			&r.Parent, &fork, &r.ForkSeq, &chainJSON, &r.Protected); err != nil {
			return nil, err
		}
		copy(r.Head[:], head)
		copy(r.ForkCommit[:], fork)
		_ = json.Unmarshal(chainJSON, &r.Chain)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteBranch removes a branch. Its versions become unreachable and are
// reclaimed by GC after a grace period (§13.2), so an accidental deletion is
// recoverable until then.
func (s *Store) DeleteBranch(ctx context.Context, repo *Repo, name string) error {
	if name == DefaultBranch {
		return fmt.Errorf("the default branch cannot be deleted")
	}
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM datagit_ref WHERE repo_id=$1 AND parent_ref=
			   (SELECT id FROM datagit_ref WHERE repo_id=$1 AND kind='branch' AND name=$2)`,
			repo.ID, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("branch %q has %d child branch(es); delete them first", name, n)
		}
		return tx.Exec(ctx,
			`DELETE FROM datagit_ref WHERE repo_id=$1 AND kind='branch' AND name=$2`, repo.ID, name)
	})
}

// --- Merge base (M2.7, §9.1) ---

// MergeBase finds the lowest common ancestors of two commits by bidirectional
// breadth-first search over the commit DAG.
//
// Returning more than one base is not an error here; §9.1 refuses the merge and
// names the candidates rather than silently picking one, because choosing wrong
// produces a result nobody notices until it matters.
func (s *Store) MergeBase(ctx context.Context, repo *Repo, a, b hash.Digest) ([]hash.Digest, error) {
	tx := s.pool.Direct()
	parents := func(d hash.Digest) ([]hash.Digest, error) {
		var ps []byte
		if err := tx.QueryRow(ctx, `SELECT parent_ids FROM datagit_commit WHERE id=$1`, d[:]).
			Scan(&ps); err != nil {
			return nil, fmt.Errorf("unknown commit %s: %w", d.Short(), err)
		}
		return decodeDigests(ps)
	}
	reach := func(start hash.Digest) (map[hash.Digest]bool, error) {
		seen := map[hash.Digest]bool{start: true}
		queue := []hash.Digest{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			ps, err := parents(cur)
			if err != nil {
				continue // a purged or pruned ancestor stops the walk
			}
			for _, p := range ps {
				if !seen[p] {
					seen[p] = true
					queue = append(queue, p)
				}
			}
		}
		return seen, nil
	}
	ra, err := reach(a)
	if err != nil {
		return nil, err
	}
	rb, err := reach(b)
	if err != nil {
		return nil, err
	}
	var common []hash.Digest
	for d := range ra {
		if rb[d] {
			common = append(common, d)
		}
	}
	// Keep only the minimal elements: drop any candidate reachable from another.
	var bases []hash.Digest
	for _, c := range common {
		dominated := false
		for _, o := range common {
			if o == c {
				continue
			}
			r, err := reach(o)
			if err == nil && r[c] {
				dominated = true
				break
			}
		}
		if !dominated {
			bases = append(bases, c)
		}
	}
	return bases, nil
}

// --- Sessions (M2.5, §6.2) ---

// Session is a lease-bound private workspace on a non-default branch.
type Session struct {
	ID         uuid.UUID
	Branch     string
	BranchID   uuid.UUID
	Base       hash.Digest
	BaseSeq    int64
	LeaseUntil time.Time
}

// DefaultLease is how long a session survives without activity.
const DefaultLease = 24 * time.Hour

// OpenSession starts a private workspace.
//
// Never on the default branch: DESIGN.md §6.1 admits no uncommitted state there,
// because the live table must be a valid materialization of main@HEAD at every
// instant for the direct readers that bypass DataGit entirely.
func (s *Store) OpenSession(ctx context.Context, repo *Repo, branch, principal string) (*Session, error) {
	if branch == DefaultBranch {
		return nil, fmt.Errorf(
			"sessions are not permitted on %s: there is no uncommitted state on the "+
				"default branch, because its live table must be a valid commit at every "+
				"instant for direct readers (§6.1). Work that should be invisible until "+
				"reviewed belongs on a branch", DefaultBranch)
	}
	var sess *Session
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		branchID, head, headSeq, _, err := s.loadRef(ctx, tx, repo, branch)
		if err != nil {
			return err
		}
		sess = &Session{ID: uuid.New(), Branch: branch, BranchID: branchID,
			Base: head, BaseSeq: headSeq, LeaseUntil: time.Now().Add(DefaultLease)}
		return tx.Exec(ctx,
			`INSERT INTO datagit_session
			   (id, repo_id, branch_id, principal, base_commit, base_seq, state, lease_until)
			 VALUES ($1,$2,$3,$4,$5,$6,'open',$7)`,
			sess.ID, repo.ID, branchID, principal, head[:], headSeq, sess.LeaseUntil)
	})
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) loadSession(ctx context.Context, tx adapter.Tx, id uuid.UUID) (*Session, error) {
	sess := &Session{ID: id}
	var base []byte
	var state string
	if err := tx.QueryRow(ctx,
		`SELECT s.branch_id, r.name, s.base_commit, s.base_seq, s.state, s.lease_until
		   FROM datagit_session s JOIN datagit_ref r ON r.id = s.branch_id
		  WHERE s.id = $1`, id).
		Scan(&sess.BranchID, &sess.Branch, &base, &sess.BaseSeq, &state, &sess.LeaseUntil); err != nil {
		return nil, fmt.Errorf("no session %s: %w", id, err)
	}
	if state != "open" {
		return nil, fmt.Errorf("session %s is %s, not open", id, state)
	}
	if time.Now().After(sess.LeaseUntil) {
		return nil, fmt.Errorf("session %s expired at %s", id, sess.LeaseUntil.Format(time.RFC3339))
	}
	copy(sess.Base[:], base)
	return sess, nil
}

// SessionWrite stages changes. Staged rows carry the session id and the zero
// commit hash, and are invisible to every read that does not name the session.
func (s *Store) SessionWrite(ctx context.Context, repo *Repo, t *Table, sid uuid.UUID, changes []Change) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		sess, err := s.loadSession(ctx, tx, sid)
		if err != nil {
			return err
		}
		view, err := s.sessionView(ctx, tx, repo, t, sess)
		if err != nil {
			return err
		}
		for _, ch := range changes {
			before, live := view[ch.PK]
			op := ch.Op
			if op == core.OpDelete {
				if !live {
					continue
				}
			} else {
				op = core.OpUpdate
				if !live {
					op = core.OpInsert
				}
				if live && before.Equal(ch.Row) {
					continue
				}
			}
			mask := core.MaskOf(before, ch.Row, t.ColIDs())

			// Upsert the session's staged row for this key. The mask accumulates,
			// so a change-and-change-back keeps its bit set — the mask is a
			// superset by contract (finding F2).
			if err := s.deleteStaged(ctx, tx, t, sid, ch.PK); err != nil {
				return err
			}
			if err := s.insertStagedVersion(ctx, tx, t, sess, sid, op, ch, mask); err != nil {
				return err
			}
			if op == core.OpDelete {
				delete(view, ch.PK)
			} else {
				view[ch.PK] = ch.Row
			}
		}
		return tx.Exec(ctx,
			`UPDATE datagit_session SET lease_until=$1, updated_at=now() WHERE id=$2`,
			time.Now().Add(DefaultLease), sid)
	})
}

func (s *Store) deleteStaged(ctx context.Context, tx adapter.Tx, t *Table, sid uuid.UUID, pk core.PK) error {
	vals, err := decodePK(pk, t)
	if err != nil {
		return err
	}
	where, args := sidecarPKWhere(t, vals, 2)
	args = append([]any{sid}, args...)
	return tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE session_id = $1 AND %s`,
		quote(catalog.SidecarTable(t.Physical)), where), args...)
}

func (s *Store) insertStagedVersion(ctx context.Context, tx adapter.Tx, t *Table,
	sess *Session, sid uuid.UUID, op core.Op, ch Change, mask core.ColMask) error {
	cols := []string{"branch_id", "seq_from", "seq_to", "op", "commit_id", "session_id", "changed_cols"}
	args := []any{sess.BranchID, sess.BaseSeq + 1, int64(9223372036854775807),
		int16(op), []byte{}, sid, maskBytes(mask)}
	for _, c := range t.Columns {
		cols = append(cols, catalog.SidecarColumn(uint32(c.ID)))
		var v any
		var err error
		if op == core.OpDelete && !containsCol(t.PKColumns, c.ID) {
			v = nil
		} else {
			v, err = bind(pkValueOf(ch, t, c.ID))
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

// sessionView resolves the session's private state: its base commit's chain plus
// its own staged rows at priority -1.
//
// Phase 0 finding F3: the base COMMIT's chain, not the branch's live head or
// even the branch's live chain. If the branch absorbs its parent while the
// session is open, the view must not change underneath the person editing in it.
func (s *Store) sessionView(ctx context.Context, tx adapter.Tx, repo *Repo, t *Table, sess *Session) (map[core.PK]core.Row, error) {
	chain, err := s.chainAt(ctx, tx, sess.Base)
	if err != nil {
		return nil, err
	}
	q, err := s.ad.ResolveQuery(&adapter.ResolveSpec{
		Table: t.Spec(), Chain: chain, Session: (*[16]byte)(&sess.ID),
	})
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("session view: %w", err)
	}
	defer rows.Close()
	out := map[core.PK]core.Row{}
	for rows.Next() {
		r, _, err := scanRow(rows, t)
		if err != nil {
			return nil, err
		}
		out[core.MakePK(r, t.PKColumns)] = r
	}
	return out, rows.Err()
}

// SessionResolve returns the session's private view as rows.
func (s *Store) SessionResolve(ctx context.Context, repo *Repo, t *Table, sid uuid.UUID) ([]core.Row, error) {
	var out []core.Row
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		sess, err := s.loadSession(ctx, tx, sid)
		if err != nil {
			return err
		}
		view, err := s.sessionView(ctx, tx, repo, t, sess)
		if err != nil {
			return err
		}
		for _, r := range view {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// CommitSession publishes a session: the staged rows are stamped with the real
// commit hash and their session id cleared. A metadata operation, not a data
// copy (§5.3), so cost is proportional to the change.
//
// It refuses if the branch moved since the session opened, which is why the
// session's view is pinned to its base rather than following the head — showing
// a view that could never be committed would be worse than refusing.
func (s *Store) CommitSession(ctx context.Context, repo *Repo, t *Table, sid uuid.UUID,
	author, message string) (*CommitResult, error) {
	res := &CommitResult{}
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		sess, err := s.loadSession(ctx, tx, sid)
		if err != nil {
			return err
		}
		if err := s.ad.AcquireRefLock(ctx, tx, sess.BranchID); err != nil {
			return err
		}
		_, head, headSeq, chain, err := s.loadRefLocked(ctx, tx, repo, sess.Branch)
		if err != nil {
			return err
		}
		if head != sess.Base {
			return fmt.Errorf(
				"branch %q moved since the session opened (head is %s, session based on %s): "+
					"re-open a session against the current head",
				sess.Branch, head.Short(), sess.Base.Short())
		}

		now, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		newSeq := headSeq + 1

		staged, err := s.stagedChanges(ctx, tx, t, sid)
		if err != nil {
			return err
		}
		if len(staged) == 0 {
			return fmt.Errorf("session %s has no staged changes", sid)
		}

		// Close the branch's own open versions for the staged keys.
		for _, ch := range staged {
			if err := s.closeOpen(ctx, tx, t, sess.BranchID, ch.PK, newSeq); err != nil {
				return err
			}
		}

		leaves := make([]hash.Change, 0, len(staged))
		for _, ch := range staged {
			leaves = append(leaves, hash.Change{
				TableID: uint64(t.ID), PK: ch.PK, Op: ch.Op, Changed: ch.Changed, Row: ch.Row,
			})
		}
		cd, err := hash.ChangeDigest(leaves, t.ColIDs())
		if err != nil {
			return err
		}
		sd := schemaDigest(t)
		id := hash.CommitID(hash.CommitInput{
			RepoID: repo.ID, Parents: []hash.Digest{head},
			ChangeDigest: cd, SchemaDigest: sd,
			Author: author, AuthorAt: now, Message: message,
		})

		// Stamp: clear the session id, set the commit and sequence.
		if err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE %s SET session_id = NULL, commit_id = $1, seq_from = $2
			  WHERE session_id = $3`,
			quote(catalog.SidecarTable(t.Physical))), id[:], newSeq, sid); err != nil {
			return fmt.Errorf("stamp staged rows: %w", err)
		}

		newChain := append([]adapter.Segment{{BranchID: sess.BranchID, Seq: newSeq}}, chain[1:]...)
		if err := insertCommit(ctx, tx, repo.ID, sess.BranchID, newSeq, id,
			[]hash.Digest{head}, author, now, message, "", cd, sd, newChain); err != nil {
			return err
		}
		if err := tx.Exec(ctx,
			`UPDATE datagit_ref SET head_commit=$1, head_seq=$2, chain=$3 WHERE id=$4`,
			id[:], newSeq, mustJSON(newChain), sess.BranchID); err != nil {
			return err
		}
		if err := tx.Exec(ctx,
			`UPDATE datagit_session SET state='committed', updated_at=now() WHERE id=$1`, sid); err != nil {
			return err
		}
		res.ID, res.Seq, res.Changed = id, newSeq, len(staged)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

type stagedChange struct {
	PK      core.PK
	Op      core.Op
	Row     core.Row
	Changed core.ColMask
}

func (s *Store) stagedChanges(ctx context.Context, tx adapter.Tx, t *Table, sid uuid.UUID) ([]stagedChange, error) {
	sel := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		sel = append(sel, quote(catalog.SidecarColumn(uint32(c.ID))))
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(
		`SELECT %s, op, changed_cols FROM %s WHERE session_id = $1 ORDER BY %s`,
		strings.Join(sel, ", "), quote(catalog.SidecarTable(t.Physical)),
		strings.Join(sel[:len(t.PKColumns)], ", ")), sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stagedChange
	for rows.Next() {
		vals := make([]any, len(t.Columns))
		dest := make([]any, 0, len(t.Columns)+2)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var op int16
		var mask []byte
		dest = append(dest, &op, &mask)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := valuesToRow(vals, t)
		sc := stagedChange{PK: core.MakePK(row, t.PKColumns), Op: core.Op(op), Changed: bytesToMask(mask)}
		if sc.Op != core.OpDelete {
			sc.Row = row
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// AbandonSession drops the staged rows. Nothing was ever visible outside the
// session, so nothing needs undoing.
func (s *Store) AbandonSession(ctx context.Context, t *Table, sid uuid.UUID) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE session_id = $1`,
			quote(catalog.SidecarTable(t.Physical))), sid); err != nil {
			return err
		}
		return tx.Exec(ctx,
			`UPDATE datagit_session SET state='abandoned', updated_at=now() WHERE id=$1`, sid)
	})
}

// ReapExpiredSessions drops staged rows for sessions whose lease has passed.
func (s *Store) ReapExpiredSessions(ctx context.Context, repo *Repo, tables []*Table) (int, error) {
	tx := s.pool.Direct()
	rows, err := tx.Query(ctx,
		`SELECT id FROM datagit_session
		  WHERE repo_id=$1 AND state='open' AND lease_until < now()`, repo.ID)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		for _, t := range tables {
			if err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE session_id=$1`,
				quote(catalog.SidecarTable(t.Physical))), id); err != nil {
				return 0, err
			}
		}
		if err := tx.Exec(ctx,
			`UPDATE datagit_session SET state='expired' WHERE id=$1`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
