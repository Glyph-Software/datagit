package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/Glyph-Software/datagit/internal/store"
)

// TestProposalReviewAndMerge is the curation loop the product exists for
// (§16.1): branch, propose, review, merge.
func TestProposalReviewAndMerge(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "q4-pricing", "TENT-4P", "outdoor", "268.92")

	p, err := f.store.CreateProposal(f.ctx, f.repo, "q4-pricing", store.DefaultBranch,
		"Q4 outdoor pricing", "Approved in FIN-2291", "arun@example.com")
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	if err := f.store.AddReview(f.ctx, f.repo, p.ID, "comment", "looks right to me", "maya@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddReview(f.ctx, f.repo, p.ID, "approve", "", "maya@example.com"); err != nil {
		t.Fatal(err)
	}

	res, err := f.store.MergeProposal(f.ctx, f.repo, f.table, p.ID, "maya@example.com")
	if err != nil {
		t.Fatalf("merge proposal: %v", err)
	}
	if !res.Clean {
		t.Fatalf("expected a clean merge, got %v", res.Conflicts)
	}

	after, err := f.store.LoadProposal(f.ctx, f.repo, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "merged" {
		t.Errorf("proposal state is %q, want merged", after.State)
	}
	if got := f.livePrice(t, "TENT-4P"); got != "268.92" {
		t.Errorf("the merged proposal did not reach the live table: %s", got)
	}
}

// TestProtectedBranchRequiresApproval (§15.3): a protected branch cannot be
// changed by one person acting alone.
func TestProtectedBranchRequiresApproval(t *testing.T) {
	f := setup(t)
	if err := f.store.SetBranchProtection(f.ctx, f.repo, store.DefaultBranch,
		store.BranchProtection{Protected: true, MinApprovals: 1}); err != nil {
		t.Fatal(err)
	}
	f.branchWith(t, "q4-pricing", "TENT-4P", "outdoor", "268.92")
	p, err := f.store.CreateProposal(f.ctx, f.repo, "q4-pricing", store.DefaultBranch,
		"Q4 pricing", "", "arun@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// No approvals yet.
	if _, err := f.store.MergeProposal(f.ctx, f.repo, f.table, p.ID, "arun@example.com"); err == nil {
		t.Fatal("merging into a protected branch without approval must be refused")
	}

	// The author's own approval is refused outright on a protected branch.
	if err := f.store.AddReview(f.ctx, f.repo, p.ID, "approve", "", "arun@example.com"); err == nil {
		t.Error("self-approval on a protected branch must be refused: review the author " +
			"can satisfy alone is not review")
	}

	// Someone else's approval unblocks it.
	if err := f.store.AddReview(f.ctx, f.repo, p.ID, "approve", "", "maya@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MergeProposal(f.ctx, f.repo, f.table, p.ID, "maya@example.com"); err != nil {
		t.Fatalf("merge after approval: %v", err)
	}
}

// TestRequestedChangesBlockMerge: an unresolved objection stops the merge.
func TestRequestedChangesBlockMerge(t *testing.T) {
	f := setup(t)
	if err := f.store.SetBranchProtection(f.ctx, f.repo, store.DefaultBranch,
		store.BranchProtection{Protected: true, MinApprovals: 1}); err != nil {
		t.Fatal(err)
	}
	f.branchWith(t, "risky", "TENT-4P", "outdoor", "999.00")
	p, err := f.store.CreateProposal(f.ctx, f.repo, "risky", store.DefaultBranch,
		"Risky pricing", "", "arun@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddReview(f.ctx, f.repo, p.ID, "approve", "", "maya@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddReview(f.ctx, f.repo, p.ID, "request_changes", "too high", "sam@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MergeProposal(f.ctx, f.repo, f.table, p.ID, "maya@example.com"); err == nil {
		t.Fatal("an unresolved request for changes must block the merge")
	}
}

// TestConflictedProposalPersistsItsConflicts (§9.4): a half-resolved merge must
// survive a restart and be resolvable by someone who was not present.
func TestConflictedProposalPersistsItsConflicts(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "cheap", "TENT-4P", "outdoor", "199.00")
	f.commitOn(t, store.DefaultBranch, "TENT-4P", "outdoor", "268.92")

	p, err := f.store.CreateProposal(f.ctx, f.repo, "cheap", store.DefaultBranch,
		"Cheaper tents", "", "arun@example.com")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.store.MergeProposal(f.ctx, f.repo, f.table, p.ID, "arun@example.com")
	if err != nil {
		t.Fatalf("merge proposal: %v", err)
	}
	if res.Clean {
		t.Fatal("expected conflicts")
	}

	after, err := f.store.LoadProposal(f.ctx, f.repo, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "conflicted" {
		t.Errorf("proposal state is %q, want conflicted", after.State)
	}
	conflicts, err := f.store.ListConflicts(f.ctx, f.table, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) == 0 {
		t.Fatal("conflicts were not persisted")
	}
	if conflicts[0].Column != "price" {
		t.Errorf("conflict names column %q, want price", conflicts[0].Column)
	}
	if err := f.store.ResolveConflict(f.ctx, conflicts[0].ID, "theirs", "199.00", "maya@example.com"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	again, err := f.store.ListConflicts(f.ctx, f.table, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again[0].Resolved {
		t.Error("the conflict was not marked resolved")
	}
	// The target is untouched by a conflicted merge.
	if got := f.livePrice(t, "TENT-4P"); got != "268.92" {
		t.Errorf("a conflicted proposal changed the live table: %s", got)
	}
}

// TestMaterializeGivesRealTables is the §7.5 escape hatch: unrestricted SQL
// against a branch, at the honest cost of a copy.
func TestMaterializeGivesRealTables(t *testing.T) {
	f := setup(t)
	f.branchWith(t, "q4", "TENT-4P", "outdoor", "268.92")

	// The it_ prefix keeps the materialization inside the namespace the test
	// user is granted, the same as any other test-created schema.
	schema := fmt.Sprintf("it_mat_%d", time.Now().UnixNano()%1000000)
	if err := f.store.Materialize(f.ctx, f.repo, "q4", schema); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	t.Cleanup(func() {
		_ = f.pool.Direct().Exec(f.ctx, f.dropSchema(schema))
	})

	// The result is an ordinary table: real column names, a primary key, and it
	// takes SQL the structured API deliberately refuses — here, an aggregate.
	var n int
	var maxPrice string
	if err := f.pool.Direct().QueryRow(f.ctx,
		fmt.Sprintf(`SELECT count(*), %s FROM "%s".products`, f.asText("max(price)"), schema)).
		Scan(&n, &maxPrice); err != nil {
		t.Fatalf("query the materialization: %v", err)
	}
	if n != 3 {
		t.Errorf("materialized %d rows, want 3", n)
	}
	if maxPrice != "268.92" {
		t.Errorf("materialized max price is %s, want the branch's 268.92", maxPrice)
	}
	// It is a snapshot of the BRANCH, not of main.
	if got := f.livePrice(t, "TENT-4P"); got != "249.00" {
		t.Errorf("materializing a branch changed the live table: %s", got)
	}
}
