package integration

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/Glyph-Software/datagit/gen/datagit/v1"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/obs"
	"github.com/Glyph-Software/datagit/internal/server"
	"github.com/Glyph-Software/datagit/internal/store"
)

// grpcFixture runs a real gRPC server over an in-memory listener, so the tests
// exercise the actual service surface rather than the store behind it.
type grpcFixture struct {
	*fixture
	conn *grpc.ClientConn
}

func setupGRPC(t *testing.T) *grpcFixture {
	t.Helper()
	f := setup(t)

	auth := server.NewAPIKeyAuth()
	auth.AddKey("arun-key", "arun@example.com", "write", "branch", "merge")
	auth.AddKey("maya-key", "maya@example.com", "write", "approve", "merge")
	auth.AddKey("reader-key", "reader@example.com", "read")
	auth.AddKey("dpo-key", "dpo@example.com", "purge")

	srv := server.New(f.store, auth, obs.New())
	g := grpc.NewServer()
	pb.RegisterRepositoryServer(g, srv)
	pb.RegisterDataServer(g, srv)
	pb.RegisterVersionServer(g, srv)
	pb.RegisterBranchingServer(g, srv)
	pb.RegisterProposalsServer(g, srv)
	pb.RegisterAdminServer(g, srv)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &grpcFixture{fixture: f, conn: conn}
}

func as(token string) context.Context {
	return metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

// TestGRPCRejectsUnauthenticatedWrites: the service surface must enforce
// authentication, not assume a client did.
func TestGRPCRejectsUnauthenticatedWrites(t *testing.T) {
	g := setupGRPC(t)
	v := pb.NewVersionClient(g.conn)

	_, err := v.Commit(context.Background(), &pb.CommitRequest{
		Repo: "catalog", Table: "products", Message: "no credential",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("an unauthenticated commit returned %v, want Unauthenticated", status.Code(err))
	}
}

// TestGRPCEnforcesCapabilities (§15.3), including that purge is NOT implied by
// any other capability.
func TestGRPCEnforcesCapabilities(t *testing.T) {
	g := setupGRPC(t)
	v := pb.NewVersionClient(g.conn)
	a := pb.NewAdminClient(g.conn)

	// A reader cannot write.
	_, err := v.Commit(as("reader-key"), &pb.CommitRequest{
		Repo: "catalog", Table: "products", Message: "should be denied",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a read-only principal writing returned %v, want PermissionDenied", status.Code(err))
	}

	// A writer cannot purge: it is a separate capability by design.
	_, err = a.Purge(as("arun-key"), &pb.PurgeRequest{
		Repo: "catalog", Table: "products", Pk: []byte(g.pk(t, "MUG-01")),
		Reason: "test",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a writer purging returned %v, want PermissionDenied: purge must not "+
			"ride along with ordinary write access (§15.3)", status.Code(err))
	}
}

// TestGRPCCommitTakesAuthorFromTheCredential is §15.2 at the wire level: the
// request has no author field, so the identity cannot be forged by a client.
func TestGRPCCommitTakesAuthorFromTheCredential(t *testing.T) {
	g := setupGRPC(t)
	v := pb.NewVersionClient(g.conn)

	row := g.row("MUG-01", "Enamel mug", "kitchen", "13.50", "2026-08-14T00:00:00Z")
	res, err := v.Commit(as("arun-key"), &pb.CommitRequest{
		Repo: "catalog", Table: "products", Branch: "main",
		Message: "priced via gRPC",
		Changes: []*pb.Change{{
			Pk: []byte(g.pk(t, "MUG-01")), Op: pb.Op_OP_UPDATE, Row: rowProto(row),
		}},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.GetRowsChanged() != 1 {
		t.Errorf("commit changed %d rows, want 1", res.GetRowsChanged())
	}

	// The recorded author is the credential's principal, which the client never
	// sent.
	commits, err := g.store.Log(g.ctx, g.repo, "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	if commits[0].Author != "arun@example.com" {
		t.Errorf("author is %q, want the authenticated principal", commits[0].Author)
	}
	if got := g.livePrice(t, "MUG-01"); got != "13.50" {
		t.Errorf("the gRPC commit did not reach the live table: %s", got)
	}
}

// TestGRPCScanAppliesFilterToResolvedRows carries the §7.3 guarantee across the
// wire: the typed predicate has no string form, so there is nothing to inject.
func TestGRPCScanAppliesFilterToResolvedRows(t *testing.T) {
	g := setupGRPC(t)
	d := pb.NewDataClient(g.conn)

	stream, err := d.Scan(as("reader-key"), &pb.ScanRequest{
		Repo: "catalog", Table: "products", Branch: "main",
		Filter: &pb.Expr{Node: &pb.Expr_Compare{Compare: &pb.Compare{
			Col: uint32(g.table.Columns[2].ID), Op: pb.CompareOp_COMPARE_OP_EQ,
			Value: &pb.Value{Kind: &pb.Value_TextValue{TextValue: "outdoor"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		r, err := stream.Recv()
		if err != nil {
			break
		}
		cat := r.GetCells()[uint32(g.table.Columns[2].ID)].GetTextValue()
		if cat != "outdoor" {
			t.Errorf("scan returned a row in category %q", cat)
		}
		n++
	}
	if n != 2 {
		t.Errorf("scan returned %d rows, want 2", n)
	}
}

// TestGRPCProtectedBranchIsEnforcedServerSide: a policy only a client honours is
// not a policy.
func TestGRPCProtectedBranchIsEnforcedServerSide(t *testing.T) {
	g := setupGRPC(t)
	br := pb.NewBranchingClient(g.conn)
	pr := pb.NewProposalsClient(g.conn)

	// Protection is set through the API by an admin-capable principal; arun is
	// not one, so set it directly and test the enforcement path.
	if err := g.store.SetBranchProtection(g.ctx, g.repo, "main",
		store.BranchProtection{Protected: true, MinApprovals: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := br.CreateBranch(as("arun-key"), &pb.CreateBranchRequest{
		Repo: "catalog", Name: "q4", From: "main",
	}); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	g.commitOn(t, "q4", "TENT-4P", "outdoor", "268.92")

	p, err := pr.CreateProposal(as("arun-key"), &pb.CreateProposalRequest{
		Repo: "catalog", From: "q4", Into: "main", Title: "Q4 pricing",
	})
	if err != nil {
		t.Fatal(err)
	}

	// No approvals: refused.
	_, err = pr.MergeProposal(as("arun-key"), &pb.MergeProposalRequest{
		Repo: "catalog", Table: "products", ProposalId: p.GetId(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging without approval returned %v, want FailedPrecondition", status.Code(err))
	}

	// The author's own approval is refused outright.
	_, err = pr.Review(as("arun-key"), &pb.ReviewRequest{
		Repo: "catalog", ProposalId: p.GetId(), Kind: "approve",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("self-approval returned %v, want PermissionDenied", status.Code(err))
	}

	// Someone else's approval unblocks it.
	if _, err := pr.Review(as("maya-key"), &pb.ReviewRequest{
		Repo: "catalog", ProposalId: p.GetId(), Kind: "approve",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	res, err := pr.MergeProposal(as("maya-key"), &pb.MergeProposalRequest{
		Repo: "catalog", Table: "products", ProposalId: p.GetId(),
	})
	if err != nil {
		t.Fatalf("merge after approval: %v", err)
	}
	if !res.GetClean() {
		t.Errorf("expected a clean merge, got %d conflicts", len(res.GetConflicts()))
	}
}

// TestGRPCVerifyStreamsFindings.
func TestGRPCVerifyStreamsFindings(t *testing.T) {
	g := setupGRPC(t)
	a := pb.NewAdminClient(g.conn)
	stream, err := a.Verify(as("reader-key"), &pb.VerifyRequest{
		Repo: "catalog", Branch: "main", Drift: true, Integrity: true, Intervals: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		f, err := stream.Recv()
		if err != nil {
			break
		}
		seen[f.GetCheck()] = true
		if !f.GetOk() {
			t.Errorf("check %s on %s failed: %s", f.GetCheck(), f.GetTable(), f.GetDetail())
		}
	}
	for _, c := range []string{"integrity", "drift", "intervals"} {
		if !seen[c] {
			t.Errorf("verify did not report the %s check", c)
		}
	}
}

func rowProto(r core.Row) *pb.Row {
	out := &pb.Row{Cells: map[uint32]*pb.Value{}}
	for _, c := range r.Cols() {
		v := r[c]
		switch v.Kind {
		case core.KindText:
			out.Cells[uint32(c)] = &pb.Value{Kind: &pb.Value_TextValue{TextValue: v.Text}}
		case core.KindNumeric:
			out.Cells[uint32(c)] = &pb.Value{Kind: &pb.Value_NumericValue{NumericValue: v.Text}}
		case core.KindInt:
			out.Cells[uint32(c)] = &pb.Value{Kind: &pb.Value_IntValue{IntValue: v.Int}}
		case core.KindTime:
			out.Cells[uint32(c)] = &pb.Value{Kind: &pb.Value_TimeValue{
				TimeValue: timestamppb.New(v.AsTime())}}
		default:
			out.Cells[uint32(c)] = &pb.Value{Kind: &pb.Value_IsNull{IsNull: true}}
		}
	}
	return out
}
