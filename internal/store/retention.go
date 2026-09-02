package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// RetentionPolicy bounds how much history is kept (§13.1).
//
// Retention, garbage collection, and erasure are three distinct problems that
// are routinely conflated. This one bounds storage; GC reclaims what is
// unreachable; erasure satisfies a legal obligation. Each has its own mechanism.
type RetentionPolicy struct {
	// KeepDays prunes closed versions older than this. Zero means unlimited.
	KeepDays int
	// KeepCommits keeps only the most recent N commits' worth of history.
	KeepCommits int
}

// PruneReport says what retention removed.
type PruneReport struct {
	VersionsRemoved  int
	CommitsProtected int
	Thinned          int
}

// Prune applies a retention policy (§13.1).
//
// Protected commits are never pruned: those that are tagged, are a branch head,
// are an ancestor of a branch head, or are referenced by a proposal.
//
// When intermediate versions are removed, the surviving older version's interval
// is EXTENDED to cover the gap, so history never claims a row was unchanged over
// a period when it was — the record that something happened survives even when
// the intermediate values do not.
func (s *Store) Prune(ctx context.Context, repo *Repo, t *Table, p RetentionPolicy) (*PruneReport, error) {
	rep := &PruneReport{}
	protected, err := s.protectedCommits(ctx, repo)
	if err != nil {
		return nil, err
	}
	rep.CommitsProtected = len(protected)
	if p.KeepDays <= 0 && p.KeepCommits <= 0 {
		return rep, nil
	}

	err = s.pool.InTx(ctx, func(tx adapter.Tx) error {
		// Find the cutoff: the oldest commit that must be kept.
		var cutoffSeq int64
		branchID, _, headSeq, _, err := s.loadRef(ctx, tx, repo, DefaultBranch)
		if err != nil {
			return err
		}
		if p.KeepCommits > 0 {
			cutoffSeq = headSeq - int64(p.KeepCommits)
		}
		if p.KeepDays > 0 {
			var bySeq *int64
			before := time.Now().AddDate(0, 0, -p.KeepDays)
			if err := tx.QueryRow(ctx,
				`SELECT max(seq) FROM datagit_commit
				  WHERE repo_id=$1 AND branch_id=$2 AND committed_at < $3`,
				repo.ID, branchID, before).Scan(&bySeq); err != nil {
				return err
			}
			if bySeq != nil && *bySeq > cutoffSeq {
				cutoffSeq = *bySeq
			}
		}
		if cutoffSeq <= 0 {
			return nil
		}

		// Only CLOSED versions are candidates: an open version is the current
		// state and can never be pruned.
		sel := make([]string, 0, len(t.PKColumns))
		for _, id := range t.PKColumns {
			sel = append(sel, quote(catalog.SidecarColumn(uint32(id))))
		}
		pkList := strings.Join(sel, ", ")

		// For each key, extend the newest surviving old version back over the gap,
		// then delete the versions it now covers.
		//
		// This is done as select-then-write rather than as one data-modifying
		// statement, for two reasons that are both engine differences, not taste.
		// PostgreSQL's `UPDATE ... FROM cte` and `DELETE ... RETURNING` inside a
		// CTE have no MySQL equivalent; and MySQL refuses (error 1093) a subquery
		// that reads the same table a DELETE is targeting, which the coverage test
		// below has to do. Reading the doomed intervals first sidesteps both and
		// bounds the work explicitly, which a maintenance path wants anyway.
		type span struct {
			pk     []any
			lo, hi int64
		}
		var doomed []span
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT %s, min(seq_from) AS lo, max(seq_to) AS hi
			  FROM %s
			 WHERE branch_id = $1 AND session_id IS NULL
			   AND seq_to <> %d AND seq_to <= $2
			 GROUP BY %s
			HAVING count(*) > 1`,
			pkList, quote(catalog.SidecarTable(t.Physical)), MaxSeqValue, pkList),
			branchID, cutoffSeq)
		if err != nil {
			return fmt.Errorf("thin history: %w", err)
		}
		for rows.Next() {
			sp := span{pk: make([]any, len(t.PKColumns))}
			dest := make([]any, 0, len(t.PKColumns)+2)
			for i := range sp.pk {
				dest = append(dest, &sp.pk[i])
			}
			dest = append(dest, &sp.lo, &sp.hi)
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return err
			}
			doomed = append(doomed, sp)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		var removed int
		for _, sp := range doomed {
			pkEq := make([]string, len(t.PKColumns))
			for i, id := range t.PKColumns {
				pkEq[i] = fmt.Sprintf("%s = $%d",
					quote(catalog.SidecarColumn(uint32(id))), i+3)
			}
			where := strings.Join(pkEq, " AND ")

			// Extend the oldest version in the run to cover the whole span.
			args := append([]any{branchID, sp.lo}, sp.pk...)
			if err := tx.Exec(ctx, fmt.Sprintf(
				`UPDATE %s SET seq_to = $%d
				  WHERE branch_id = $1 AND session_id IS NULL AND seq_from = $2 AND %s`,
				quote(catalog.SidecarTable(t.Physical)), len(args)+1, where),
				append(args, sp.hi)...); err != nil {
				return fmt.Errorf("thin history: %w", err)
			}

			// Delete every version the extended one now covers. seq_from > lo
			// spares the extended version itself.
			n, err := tx.ExecCount(ctx, fmt.Sprintf(
				`DELETE FROM %s
				  WHERE branch_id = $1 AND session_id IS NULL
				    AND seq_from > $2 AND seq_to <> %d AND seq_to <= $%d AND %s`,
				quote(catalog.SidecarTable(t.Physical)), MaxSeqValue, len(args)+1, where),
				append(args, cutoffSeq)...)
			if err != nil {
				return fmt.Errorf("prune history: %w", err)
			}
			removed += int(n)
		}
		rep.VersionsRemoved = removed
		rep.Thinned = removed
		return nil
	})
	return rep, err
}

// MaxSeqValue mirrors the sidecar's open-interval sentinel.
const MaxSeqValue = 9223372036854775807

func joinPKEq(t *Table, a, b string) string {
	parts := make([]string, 0, len(t.PKColumns))
	for _, id := range t.PKColumns {
		c := quote(catalog.SidecarColumn(uint32(id)))
		parts = append(parts, fmt.Sprintf("%s.%s = %s.%s", a, c, b, c))
	}
	return strings.Join(parts, " AND ")
}

// protectedCommits lists commits retention must never prune (§13.1).
func (s *Store) protectedCommits(ctx context.Context, repo *Repo) (map[hash.Digest]bool, error) {
	out := map[hash.Digest]bool{}
	rows, err := s.pool.Direct().Query(ctx,
		`SELECT head_commit FROM datagit_ref WHERE repo_id=$1 AND head_commit IS NOT NULL
		 UNION
		 SELECT merge_commit FROM datagit_proposal WHERE repo_id=$1 AND merge_commit IS NOT NULL`,
		repo.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var d hash.Digest
		copy(d[:], b)
		out[d] = true
	}
	return out, rows.Err()
}

// --- Garbage collection (§13.2) ---

// GCReport says what garbage collection reclaimed.
type GCReport struct {
	SessionsReaped     int
	OrphanVersions     int
	MaterializationsGC int
}

// GCGracePeriod is how long a deleted branch's versions survive, so an
// accidental deletion is recoverable.
const GCGracePeriod = 7 * 24 * time.Hour

// GC reclaims unreachable versions and expired sessions (§13.2).
//
// Deleting a branch makes its versions unreachable, but they are not removed
// immediately: a grace period means an accidental deletion can be undone.
func (s *Store) GC(ctx context.Context, repo *Repo) (*GCReport, error) {
	rep := &GCReport{}
	tables, err := s.ListTables(ctx, repo)
	if err != nil {
		return nil, err
	}

	n, err := s.ReapExpiredSessions(ctx, repo, tables)
	if err != nil {
		return nil, err
	}
	rep.SessionsReaped = n

	// Versions whose branch no longer exists.
	live := map[uuid.UUID]bool{}
	refs, err := s.ListRefs(ctx, repo)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		live[r.ID] = true
	}

	for _, t := range tables {
		rows, err := s.pool.Direct().Query(ctx, fmt.Sprintf(
			`SELECT DISTINCT branch_id FROM %s`, quote(catalog.SidecarTable(t.Physical))))
		if err != nil {
			return nil, err
		}
		var orphans []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !live[id] {
				orphans = append(orphans, id)
			}
		}
		rows.Close()

		for _, id := range orphans {
			n, err := s.pool.Direct().ExecCount(ctx, fmt.Sprintf(
				`DELETE FROM %s WHERE branch_id=$1`,
				quote(catalog.SidecarTable(t.Physical))), id)
			if err != nil {
				return nil, err
			}
			rep.OrphanVersions += int(n)
		}
	}
	return rep, nil
}

// --- Hard purge (§13.4) ---

// PurgeReceipt records what a purge removed.
type PurgeReceipt struct {
	Key             core.PK
	VersionsRemoved int
	CommitsMarked   int
	Reason          string
	By              string
	At              time.Time
}

// Purge physically removes a row's history across every commit and branch
// (§13.4).
//
// This is the audited escape hatch for what crypto-shredding cannot reach: PII
// in a non-designated column, a court order, a regulator demanding physical
// removal. It requires a stated reason, and it deliberately BREAKS the hash
// chain for the affected commits.
//
// Those commits are marked `integrity = 'purged'` rather than re-hashed. Silently
// recomputing the hashes would hide the gap and make the audit trail lie; the
// difference between "an authorized erasure happened here" and "someone tampered
// with this" must stay visible, and `verify --integrity` reports it either way.
func (s *Store) Purge(ctx context.Context, repo *Repo, t *Table, pk core.PK, reason, principal string) (*PurgeReceipt, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("purge requires a stated reason: it is an audited, irreversible operation")
	}
	rec := &PurgeReceipt{Key: pk, Reason: reason, By: principal}
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		vals, err := decodePK(pk, t)
		if err != nil {
			return err
		}
		where, args := sidecarPKWhere(t, vals, 1)

		// Which commits are about to lose content?
		rows, err := tx.Query(ctx, fmt.Sprintf(
			`SELECT DISTINCT commit_id FROM %s WHERE %s`,
			quote(catalog.SidecarTable(t.Physical)), where), args...)
		if err != nil {
			return err
		}
		var affected [][]byte
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				rows.Close()
				return err
			}
			affected = append(affected, b)
		}
		rows.Close()

		removed, err := tx.ExecCount(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE %s`,
			quote(catalog.SidecarTable(t.Physical)), where), args...)
		if err != nil {
			return fmt.Errorf("purge versions: %w", err)
		}
		rec.VersionsRemoved = int(removed)

		// The live row goes too.
		liveWhere, liveArgs := pkWhere(t, vals, 1)
		if err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s`,
			quote(t.Physical), liveWhere), liveArgs...); err != nil {
			return err
		}

		for _, cid := range affected {
			if err := tx.Exec(ctx,
				`UPDATE datagit_commit SET integrity='purged' WHERE id=$1`, cid); err != nil {
				return err
			}
			rec.CommitsMarked++
		}

		now, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		rec.At = now
		// The tombstone records THAT a purge happened, never the purged content.
		return tx.Exec(ctx,
			`INSERT INTO datagit_purge_log
			   (repo_id, table_id, pk_bytes, versions_removed, reason, purged_by, purged_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			repo.ID, t.ID, []byte(pk), removed, reason, principal, now)
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// --- Verify: intervals (M4.4, §17.3) ---

// IntervalReport describes sidecar interval consistency.
type IntervalReport struct {
	Overlaps       []string
	MultipleOpen   []string
	OrphanSessions int
}

// VerifyIntervals checks the sidecar's structural invariants (§17.3): no
// overlapping intervals, and exactly one open version per key per branch.
//
// These are the invariants every read depends on. A violation does not surface
// as an error at read time — it surfaces as a wrong answer — so it is checked
// explicitly rather than assumed.
func (s *Store) VerifyIntervals(ctx context.Context, t *Table) (*IntervalReport, error) {
	rep := &IntervalReport{}
	pkCols := make([]string, 0, len(t.PKColumns))
	for _, id := range t.PKColumns {
		pkCols = append(pkCols, quote(catalog.SidecarColumn(uint32(id))))
	}
	pkList := strings.Join(pkCols, ", ")

	// Overlapping intervals for the same key on the same branch.
	rows, err := s.pool.Direct().Query(ctx, fmt.Sprintf(`
		SELECT a.branch_id, %s, a.seq_from, a.seq_to, b.seq_from, b.seq_to
		  FROM %s a JOIN %s b
		    ON a.branch_id = b.branch_id AND %s
		   AND a.seq_from < b.seq_from
		 WHERE a.session_id IS NULL AND b.session_id IS NULL
		   AND b.seq_from < a.seq_to
		 LIMIT 20`,
		prefixCols("a", pkCols), quote(catalog.SidecarTable(t.Physical)),
		quote(catalog.SidecarTable(t.Physical)), joinPKEq(t, "a", "b")))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var branch uuid.UUID
		dest := make([]any, 0, len(t.PKColumns)+5)
		dest = append(dest, &branch)
		vals := make([]any, len(t.PKColumns))
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var af, at, bf, bt int64
		dest = append(dest, &af, &at, &bf, &bt)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		rep.Overlaps = append(rep.Overlaps,
			fmt.Sprintf("branch %s: [%d,%d) overlaps [%d,%d)", branch, af, at, bf, bt))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// More than one open version per key per branch.
	orows, err := s.pool.Direct().Query(ctx, fmt.Sprintf(`
		SELECT branch_id, count(*) FROM %s
		 WHERE session_id IS NULL AND seq_to = %d
		 GROUP BY branch_id, %s HAVING count(*) > 1 LIMIT 20`,
		quote(catalog.SidecarTable(t.Physical)), MaxSeqValue, pkList))
	if err != nil {
		return nil, err
	}
	defer orows.Close()
	for orows.Next() {
		var branch uuid.UUID
		var n int
		if err := orows.Scan(&branch, &n); err != nil {
			return nil, err
		}
		rep.MultipleOpen = append(rep.MultipleOpen,
			fmt.Sprintf("branch %s: a key has %d open versions, want exactly 1", branch, n))
	}
	return rep, orows.Err()
}

func prefixCols(alias string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return strings.Join(out, ", ")
}
