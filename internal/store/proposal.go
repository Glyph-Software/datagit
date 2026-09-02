package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// --- Materialization (M2.10, §7.5) ---

// Materialize writes a branch's resolved state into a new schema as ordinary
// tables (§7.5).
//
// The escape hatch that lets §1.3's "not a query engine" stand. The result takes
// unrestricted SQL — joins, aggregates, a BI tool, the application's own ORM —
// at the honest cost of being a point-in-time copy that cannot be written back.
func (s *Store) Materialize(ctx context.Context, repo *Repo, branch, into string) error {
	tables, err := s.ListTables(ctx, repo)
	if err != nil {
		return err
	}
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		_, _, _, chain, err := s.loadRef(ctx, tx, repo, branch)
		if err != nil {
			return err
		}
		if err := tx.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, quote(into))); err != nil {
			return fmt.Errorf("create materialization schema %q: %w", into, err)
		}
		for _, t := range tables {
			if err := s.ad.MaterializeBranch(ctx, tx, chain, t.Spec(), into); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Proposals (M3.6, §16.1) ---

// Proposal is a reviewable request to merge one branch into another.
type Proposal struct {
	ID          int64
	RepoID      string
	From, Into  string
	Title       string
	Description string
	State       string
	MergeCommit hash.Digest
	CreatedBy   string
	CreatedAt   time.Time
}

// Review is a comment or an approval on a proposal.
type Review struct {
	ID        int64
	Principal string
	Kind      string
	Body      string
	CreatedAt time.Time
}

// CreateProposal opens a review of a branch-to-branch merge.
func (s *Store) CreateProposal(ctx context.Context, repo *Repo, from, into, title, description, principal string) (*Proposal, error) {
	p := &Proposal{From: from, Into: into, Title: title, Description: description,
		State: "open", CreatedBy: principal}
	err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		// created_at comes from the DATABASE clock, read here and inserted
		// explicitly rather than left to a column default. A default would have to
		// be read back, and RETURNING is PostgreSQL-only (§7.2, §4.3).
		at, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		id, err := s.ad.InsertReturningID(ctx, tx,
			`INSERT INTO datagit_proposal
			   (repo_id, from_ref, into_ref, title, description, state, created_by, created_at)
			 SELECT $1, f.id, i.id, $4, $5, 'open', $6, $7
			   FROM datagit_ref f, datagit_ref i
			  WHERE f.repo_id=$1 AND f.kind='branch' AND f.name=$2
			    AND i.repo_id=$1 AND i.kind='branch' AND i.name=$3`,
			repo.ID, from, into, title, description, principal, at)
		if err != nil {
			return err
		}
		if id == 0 {
			return fmt.Errorf("branch %q or %q does not exist", from, into)
		}
		p.ID, p.CreatedAt = id, at
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create proposal: %w", err)
	}
	return p, nil
}

// AddReview records a comment, an approval, or a request for changes.
//
// Self-approval is refused when the branch requires it (§15.3). Review that the
// author can satisfy alone is not review.
func (s *Store) AddReview(ctx context.Context, repo *Repo, proposalID int64, kind, body, principal string) error {
	switch kind {
	case "comment", "approve", "request_changes":
	default:
		return fmt.Errorf("review kind must be comment, approve, or request_changes")
	}
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if kind == "approve" {
			var author string
			var selfApprovalAllowed bool
			if err := tx.QueryRow(ctx,
				`SELECT p.created_by, NOT i.protected
				   FROM datagit_proposal p JOIN datagit_ref i ON i.id = p.into_ref
				  WHERE p.id = $1`, proposalID).Scan(&author, &selfApprovalAllowed); err != nil {
				return err
			}
			if author == principal && !selfApprovalAllowed {
				return fmt.Errorf(
					"%s opened this proposal and cannot approve it: the target branch is "+
						"protected, and review the author can satisfy alone is not review (§15.3)",
					principal)
			}
		}
		return tx.Exec(ctx,
			`INSERT INTO datagit_review (proposal_id, principal, kind, body) VALUES ($1,$2,$3,$4)`,
			proposalID, principal, kind, body)
	})
}

// LoadProposal reads a proposal with its branch names.
func (s *Store) LoadProposal(ctx context.Context, repo *Repo, id int64) (*Proposal, error) {
	p := &Proposal{ID: id}
	var merge []byte
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT f.name, i.name, p.title, p.description, p.state,
		        p.merge_commit, p.created_by, p.created_at
		   FROM datagit_proposal p
		   JOIN datagit_ref f ON f.id = p.from_ref
		   JOIN datagit_ref i ON i.id = p.into_ref
		  WHERE p.id = $1 AND p.repo_id = $2`, id, repo.ID).
		Scan(&p.From, &p.Into, &p.Title, &p.Description, &p.State, &merge, &p.CreatedBy, &p.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("no proposal %d: %w", id, err)
	}
	copy(p.MergeCommit[:], merge)
	return p, nil
}

// ListReviews returns a proposal's comments and approvals.
func (s *Store) ListReviews(ctx context.Context, id int64) ([]Review, error) {
	rows, err := s.pool.Direct().Query(ctx,
		`SELECT id, principal, kind, body, created_at FROM datagit_review
		  WHERE proposal_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.Principal, &r.Kind, &r.Body, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BranchProtection is a branch's merge policy (§15.3).
type BranchProtection struct {
	Protected    bool
	MinApprovals int
}

// SetBranchProtection configures a branch's merge policy.
func (s *Store) SetBranchProtection(ctx context.Context, repo *Repo, branch string, p BranchProtection) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx,
			`UPDATE datagit_ref SET protected=$1, min_approvals=$2
			  WHERE repo_id=$3 AND kind='branch' AND name=$4`,
			p.Protected, p.MinApprovals, repo.ID, branch)
	})
}

// MergeProposal validates the branch's protection rules, then merges (§15.3).
//
// The rules exist so that a protected branch cannot be changed by one person
// acting alone. They are checked here rather than in a UI, because a policy only
// enforced by a client is not a policy.
func (s *Store) MergeProposal(ctx context.Context, repo *Repo, t *Table, id int64, principal string) (*MergeResult, error) {
	p, err := s.LoadProposal(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	if p.State == "merged" {
		return nil, fmt.Errorf("proposal %d is already merged", id)
	}

	var protected bool
	var minApprovals int
	if err := s.pool.Direct().QueryRow(ctx,
		`SELECT protected, min_approvals FROM datagit_ref
		  WHERE repo_id=$1 AND kind='branch' AND name=$2`, repo.ID, p.Into).
		Scan(&protected, &minApprovals); err != nil {
		return nil, err
	}

	if protected {
		reviews, err := s.ListReviews(ctx, id)
		if err != nil {
			return nil, err
		}
		approvals := map[string]bool{}
		for _, r := range reviews {
			switch r.Kind {
			case "approve":
				if r.Principal != p.CreatedBy {
					approvals[r.Principal] = true
				}
			case "request_changes":
				return nil, fmt.Errorf(
					"proposal %d has unresolved requested changes from %s", id, r.Principal)
			}
		}
		if len(approvals) < minApprovals {
			return nil, fmt.Errorf(
				"branch %q is protected and requires %d approval(s); proposal %d has %d "+
					"(the author's own approval does not count)",
				p.Into, minApprovals, id, len(approvals))
		}
	}

	// Schema merges BEFORE data, because the data merge needs to know the shape
	// it is producing (§10.3).
	sm, err := s.MergeSchema(ctx, repo, t, p.From, p.Into, id, principal)
	if err != nil {
		return nil, err
	}
	if len(sm.Conflicts) > 0 {
		// A shape disagreement blocks the whole merge. Merging the data into a
		// shape nobody has agreed on would produce rows that fit neither branch.
		if err := s.setProposalState(ctx, id, "conflicted", hash.Digest{}); err != nil {
			return nil, err
		}
		return &MergeResult{SchemaConflicts: sm.Conflicts}, nil
	}

	res, err := s.Merge(ctx, repo, t, p.From, p.Into, principal,
		fmt.Sprintf("Merge proposal #%d: %s", id, p.Title), true)
	if err != nil {
		return nil, err
	}
	res.PendingMigration = sm.Plan

	if !res.Clean {
		// Conflicts are persisted so the proposal can be resolved over days, by
		// someone who was not present when the merge was attempted (§9.4).
		if err := s.clearConflicts(ctx, id); err != nil {
			return nil, err
		}
		if err := s.SaveConflicts(ctx, id, t, res.Conflicts); err != nil {
			return nil, err
		}
		if err := s.setProposalState(ctx, id, "conflicted", hash.Digest{}); err != nil {
			return nil, err
		}
		return res, nil
	}
	if err := s.setProposalState(ctx, id, "merged", res.Commit); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Store) clearConflicts(ctx context.Context, id int64) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx, `DELETE FROM datagit_conflict WHERE proposal_id=$1`, id)
	})
}

func (s *Store) setProposalState(ctx context.Context, id int64, state string, merge hash.Digest) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if merge.IsZero() {
			return tx.Exec(ctx, `UPDATE datagit_proposal SET state=$1 WHERE id=$2`, state, id)
		}
		return tx.Exec(ctx,
			`UPDATE datagit_proposal SET state=$1, merge_commit=$2 WHERE id=$3`,
			state, merge[:], id)
	})
}

// ConflictRow is a persisted conflict as a reviewer sees it.
type ConflictRow struct {
	ID       int64
	PK       core.PK
	Column   string
	Kind     string
	Base     string
	Ours     string
	Theirs   string
	Resolved bool
}

// ListConflicts returns a proposal's outstanding conflicts.
func (s *Store) ListConflicts(ctx context.Context, t *Table, id int64) ([]ConflictRow, error) {
	rows, err := s.pool.Direct().Query(ctx,
		`SELECT id, pk_bytes, column_id, kind,
		        coalesce(base_value,''), coalesce(our_value,''), coalesce(their_value,''),
		        resolution IS NOT NULL
		   FROM datagit_conflict WHERE proposal_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConflictRow
	for rows.Next() {
		var c ConflictRow
		var pk []byte
		var col *int32
		if err := rows.Scan(&c.ID, &pk, &col, &c.Kind, &c.Base, &c.Ours, &c.Theirs, &c.Resolved); err != nil {
			return nil, err
		}
		c.PK = core.PK(pk)
		if col != nil {
			if cc, ok := t.Column(core.ColID(*col)); ok {
				c.Column = cc.Name
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveConflict records a decision on one conflict (§9.4).
func (s *Store) ResolveConflict(ctx context.Context, conflictID int64, resolution, value, principal string) error {
	switch resolution {
	case "ours", "theirs", "custom":
	default:
		return fmt.Errorf("resolution must be ours, theirs, or custom")
	}
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx,
			`UPDATE datagit_conflict
			    SET resolution=$1, resolved_value=$2, resolved_by=$3, resolved_at=now()
			  WHERE id=$4`, resolution, value, principal, conflictID)
	})
}

// ListProposals returns a repository's proposals, newest first.
func (s *Store) ListProposals(ctx context.Context, repo *Repo, states ...string) ([]Proposal, error) {
	q := `SELECT p.id, f.name, i.name, p.title, p.state, p.created_by, p.created_at
	        FROM datagit_proposal p
	        JOIN datagit_ref f ON f.id = p.from_ref
	        JOIN datagit_ref i ON i.id = p.into_ref
	       WHERE p.repo_id = $1`
	args := []any{repo.ID}
	if len(states) > 0 {
		cond, a := inList("p.state", states, len(args)+1)
		q += ` AND ` + cond
		args = append(args, a...)
	}
	q += ` ORDER BY p.id DESC`

	rows, err := s.pool.Direct().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.From, &p.Into, &p.Title, &p.State,
			&p.CreatedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

var _ = strings.Join
