package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// DefaultAtomicApplyLimit is how many changed rows a merge may apply in one
// transaction (§9.5).
//
// A single transaction is the right default and the wrong tool past a size: a
// million-row change set holds a million row locks on a production table,
// generates a proportionate WAL burst, and stalls replicas behind it.
const DefaultAtomicApplyLimit = 50000

// ErrMergeTooLarge is returned when a merge exceeds the atomic apply limit and
// the caller has not opted into chunked apply.
type ErrMergeTooLarge struct {
	Rows  int
	Limit int
}

func (e *ErrMergeTooLarge) Error() string {
	return fmt.Sprintf(
		"merge would change %d rows, over the atomic apply limit of %d: "+
			"opt into chunked apply, which relaxes atomicity visibly and is bounded, "+
			"or split the change (§9.5)", e.Rows, e.Limit)
}

// ChunkedApplyOptions configures a chunked merge.
type ChunkedApplyOptions struct {
	// ChunkSize is how many keys each transaction applies.
	ChunkSize int
	// Progress is called after each chunk, for operator visibility.
	Progress func(applied, total int)
}

// ApplyChunked applies a change set in primary-key-ordered chunks, each its own
// transaction, journalled so a crash resumes rather than restarts (§9.5).
//
// This RELAXES the §11.1 atomicity guarantee, and does so visibly: the ref
// carries a `merge_in_progress` flag for the duration, so anyone reading
// datagit_ref knows the live table is mid-transition. During the apply a direct
// reader can observe a state that is not a commit.
//
// The relaxation is bounded, which is what makes it acceptable: every
// intermediate state is "old value for keys not yet applied, new value for keys
// already applied", never anything else. A restatement large enough to need this
// should be planned as a change window, the way a large migration would be.
func (s *Store) ApplyChunked(ctx context.Context, repo *Repo, t *Table, branch string,
	changes []Change, parents []hash.Digest, author, message string,
	opt ChunkedApplyOptions) (*CommitResult, error) {

	if opt.ChunkSize <= 0 {
		opt.ChunkSize = 5000
	}
	sorted := append([]Change(nil), changes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PK < sorted[j].PK })

	// Flag the ref before touching anything, so the in-progress state is visible
	// from the moment it becomes possible to observe a partial merge.
	if err := s.setMergeInProgress(ctx, repo, branch, true); err != nil {
		return nil, err
	}
	defer func() { _ = s.setMergeInProgress(ctx, repo, branch, false) }()

	var applied int
	for start := 0; start < len(sorted); start += opt.ChunkSize {
		end := start + opt.ChunkSize
		if end > len(sorted) {
			end = len(sorted)
		}
		chunk := sorted[start:end]

		// Each chunk applies the live-table writes only. Version records and the
		// commit come at the end, so history records one merge rather than N
		// arbitrary slices of one.
		if err := s.applyLiveChunk(ctx, repo, t, branch, chunk); err != nil {
			return nil, fmt.Errorf("chunked apply at row %d: %w", start, err)
		}
		applied += len(chunk)
		if opt.Progress != nil {
			opt.Progress(applied, len(sorted))
		}
	}

	// The commit closes the window: after it, the branch is a valid commit again.
	res, err := s.Commit(ctx, CommitRequest{
		Repo: repo, Table: t, Branch: branch, Changes: sorted,
		Author: author, Message: message, ExtraParents: parents,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Store) applyLiveChunk(ctx context.Context, repo *Repo, t *Table, branch string, chunk []Change) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if err := MarkWriter(ctx, tx); err != nil {
			return err
		}
		branchID, _, _, _, err := s.loadRef(ctx, tx, repo, branch)
		if err != nil {
			return err
		}
		if branchID != repo.DefaultBranch {
			return nil // only the default branch has a live table to write
		}
		for _, ch := range chunk {
			// applyLive upserts, so the live/not-live distinction only selects
			// between a delete and an upsert.
			if err := s.applyLive(ctx, tx, t, ch, ch.Op, ch.Op != core.OpInsert); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) setMergeInProgress(ctx context.Context, repo *Repo, branch string, on bool) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx,
			`UPDATE datagit_ref SET merge_in_progress = $1
			  WHERE repo_id = $2 AND kind = 'branch' AND name = $3`,
			on, repo.ID, branch)
	})
}

// MergeInProgress reports whether a branch is mid-chunked-apply, so operators
// and readers can tell that the live table may not currently be a commit.
func (s *Store) MergeInProgress(ctx context.Context, repo *Repo, branch string) (bool, error) {
	var on bool
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT merge_in_progress FROM datagit_ref
		  WHERE repo_id=$1 AND kind='branch' AND name=$2`, repo.ID, branch).Scan(&on)
	return on, err
}

// MergeLarge is Merge with an explicit size policy (§9.5).
//
// Above the atomic limit it REFUSES unless chunked is requested. Silently
// chunking would relax an advertised guarantee without anyone deciding to.
func (s *Store) MergeLarge(ctx context.Context, repo *Repo, t *Table, from, into, author, message string,
	chunked bool, limit int, progress func(applied, total int)) (*MergeResult, error) {

	if limit <= 0 {
		limit = DefaultAtomicApplyLimit
	}
	// Compute without applying, so the size is known before anything is written.
	res, err := s.Merge(ctx, repo, t, from, into, author, message, false)
	if err != nil {
		return nil, err
	}
	if !res.Clean {
		return res, nil
	}
	if len(res.Changes) <= limit {
		return s.Merge(ctx, repo, t, from, into, author, message, true)
	}
	if !chunked {
		return nil, &ErrMergeTooLarge{Rows: len(res.Changes), Limit: limit}
	}

	_, fromHead, _, _, err := s.loadRef(ctx, s.pool.Direct(), repo, from)
	if err != nil {
		return nil, err
	}
	if message == "" {
		message = fmt.Sprintf("Merge %s into %s (chunked)", from, into)
	}
	cr, err := s.ApplyChunked(ctx, repo, t, into, res.Changes,
		[]hash.Digest{fromHead}, author, message,
		ChunkedApplyOptions{Progress: progress})
	if err != nil {
		return nil, err
	}
	res.Commit, res.Applied = cr.ID, cr.Changed
	return res, nil
}
