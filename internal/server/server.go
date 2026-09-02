// Package server exposes DataGit over gRPC (DESIGN.md §16).
//
// Two rules shape every handler here:
//
//   - The author of a commit comes from the AUTHENTICATED PRINCIPAL, never from
//     the request (§15.2). CommitRequest has no author field, and never will:
//     an audit trail whose author is client-supplied is decoration.
//   - Policy is enforced here, not in a client. A protected branch that a UI
//     merely declines to offer a merge button for is not protected.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/Glyph-Software/datagit/gen/datagit/v1"
	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
	"github.com/Glyph-Software/datagit/internal/obs"
	"github.com/Glyph-Software/datagit/internal/store"
)

// Authenticator resolves a request's principal. It is deliberately an interface:
// API keys land first, OIDC follows, and neither should reach into the handlers.
type Authenticator interface {
	// Principal returns the authenticated identity, or an error if the request
	// carries no valid credential.
	Principal(ctx context.Context) (string, error)
	// Can reports whether a principal holds a capability (§15.3).
	Can(ctx context.Context, principal, capability string) bool
}

// Server implements every DataGit service.
type Server struct {
	store   *store.Store
	auth    Authenticator
	metrics *obs.Metrics
}

func New(st *store.Store, auth Authenticator, m *obs.Metrics) *Server {
	if m == nil {
		m = obs.New()
	}
	return &Server{store: st, auth: auth, metrics: m}
}

// principal resolves the caller, or fails the RPC. Every mutating handler calls
// this; none of them read an author from the request.
func (s *Server) principal(ctx context.Context) (string, error) {
	p, err := s.auth.Principal(ctx)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, err.Error())
	}
	return p, nil
}

func (s *Server) require(ctx context.Context, capability string) (string, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return "", err
	}
	if !s.auth.Can(ctx, p, capability) {
		return "", status.Errorf(codes.PermissionDenied,
			"%s lacks the %q capability", p, capability)
	}
	return p, nil
}

// --- Repository ---

func (s *Server) CreateRepo(ctx context.Context, req *pb.CreateRepoRequest) (*pb.RepoInfo, error) {
	p, err := s.require(ctx, "admin")
	if err != nil {
		return nil, err
	}
	if err := s.store.InitControlSchema(ctx); err != nil {
		return nil, internal(err)
	}
	repo, err := s.store.CreateRepo(ctx, req.GetName(), p)
	if err != nil {
		return nil, internal(err)
	}
	return &pb.RepoInfo{Id: repo.ID.String(), Name: repo.Name,
		DefaultBranch: store.DefaultBranch}, nil
}

func (s *Server) TrackTable(ctx context.Context, req *pb.TrackTableRequest) (*pb.TableInfo, error) {
	if _, err := s.require(ctx, "admin"); err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), "")
	if err != nil {
		return nil, err
	}
	_ = t
	mode := adapter.Mode(req.GetMode())
	if mode == "" {
		mode = adapter.ModeVersioned
	}
	tbl, err := s.store.Track(ctx, repo, req.GetTable(), mode)
	if err != nil {
		// A refusal here is a deliberate design decision (no primary key, an
		// unmirrorable type), so it maps to FailedPrecondition with the reason
		// intact rather than a generic error.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return tableInfo(tbl), nil
}

func (s *Server) UntrackTable(ctx context.Context, req *pb.UntrackTableRequest) (*pb.Empty, error) {
	if _, err := s.require(ctx, "admin"); err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	if err := s.store.Untrack(ctx, repo, t); err != nil {
		return nil, internal(err)
	}
	return &pb.Empty{}, nil
}

func (s *Server) GetStatus(ctx context.Context, req *pb.GetStatusRequest) (*pb.RepoStatus, error) {
	if _, err := s.require(ctx, "read"); err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	tables, err := s.store.ListTables(ctx, repo)
	if err != nil {
		return nil, internal(err)
	}
	refs, err := s.store.ListRefs(ctx, repo)
	if err != nil {
		return nil, internal(err)
	}
	out := &pb.RepoStatus{Repo: &pb.RepoInfo{Id: repo.ID.String(), Name: repo.Name}}
	for _, t := range tables {
		out.Tables = append(out.Tables, tableInfo(t))
	}
	for _, r := range refs {
		out.Refs = append(out.Refs, refInfo(r))
	}
	return out, nil
}

// --- Version: the write path ---

func (s *Server) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	p, err := s.require(ctx, "write")
	if err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	changes, err := changesFromProto(req.GetChanges())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	cr := store.CommitRequest{
		Repo: repo, Table: t, Branch: branchOr(req.GetBranch()),
		Changes: changes,
		// From the authenticated principal. The request has no author field.
		Author:      p,
		Message:     req.GetMessage(),
		ExternalRef: req.GetExternalRef(),
	}
	if h := req.GetExpectedHead(); len(h) == hash.Size {
		var d hash.Digest
		copy(d[:], h)
		cr.ExpectedHead = &d
	}

	started := time.Now()
	res, err := s.store.Commit(ctx, cr)
	if err != nil {
		s.metrics.CommitFailed()
		// A moved head is the caller's to retry against the new state, not a
		// server fault.
		if isPrecondition(err) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, internal(err)
	}
	s.metrics.CommitOK(res.Changed, time.Since(started))
	return &pb.CommitResponse{Id: res.ID[:], Seq: res.Seq, RowsChanged: int32(res.Changed)}, nil
}

func (s *Server) Log(req *pb.LogRequest, srv pb.Version_LogServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return notFound(err)
	}
	commits, err := s.store.Log(ctx, repo, branchOr(req.GetBranch()), int(req.GetLimit()))
	if err != nil {
		return internal(err)
	}
	for _, c := range commits {
		id := c.ID
		if err := srv.Send(&pb.CommitInfo{
			Id: id[:], Seq: c.Seq, Author: c.Author,
			CommittedAt: timestamppb.New(c.CommittedAt),
			Message:     c.Message, ExternalRef: c.ExternalRef, Integrity: c.Integrity,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) Blame(req *pb.BlameRequest, srv pb.Version_BlameServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return err
	}
	var cols []core.ColID
	for _, c := range req.GetColumns() {
		cols = append(cols, core.ColID(c))
	}
	blame, err := s.store.Blame(ctx, repo, t, branchOr(req.GetBranch()),
		core.PK(req.GetPk()), cols)
	if err != nil {
		return internal(err)
	}
	for _, b := range blame {
		id := b.CommitID
		if err := srv.Send(&pb.CellBlame{
			Col: uint32(b.Col), Value: valueToProto(b.Value), CommitId: id[:],
			Author: b.Author, At: timestamppb.New(b.At), Message: b.Message,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) Revert(ctx context.Context, req *pb.RevertRequest) (*pb.CommitResponse, error) {
	p, err := s.require(ctx, "write")
	if err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	var d hash.Digest
	copy(d[:], req.GetCommitId())
	res, err := s.store.Revert(ctx, repo, t, branchOr(req.GetBranch()), d, p,
		req.GetMessage(), req.GetForce())
	if err != nil {
		// Refusing to discard later work is a deliberate decision, not a fault.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.CommitResponse{Id: res.ID[:], Seq: res.Seq, RowsChanged: int32(res.Changed)}, nil
}

// --- Data ---

func (s *Server) Scan(req *pb.ScanRequest, srv pb.Data_ScanServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return err
	}
	filter, err := exprFromProto(req.GetFilter())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	opt := store.ReadOptions{
		Branch: branchOr(req.GetBranch()), Filter: filter,
		Limit: int(req.GetLimit()), After: core.PK(req.GetAfter()),
	}
	if h := req.GetAtCommit(); len(h) == hash.Size {
		var d hash.Digest
		copy(d[:], h)
		opt.At = &d
	}
	if ts := req.GetAsOf(); ts != nil {
		v := ts.AsTime()
		opt.AsOf = &v
	}
	started := time.Now()
	rows, err := s.store.Read(ctx, repo, t, opt)
	if err != nil {
		return internal(err)
	}
	s.metrics.Read(1, time.Since(started))
	for _, r := range rows {
		if err := srv.Send(rowToProto(r)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) Get(ctx context.Context, req *pb.GetRequest) (*pb.Row, error) {
	if _, err := s.require(ctx, "read"); err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	kf, err := keyExpr(t, core.PK(req.GetPk()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	rows, err := s.store.Read(ctx, repo, t, store.ReadOptions{
		Branch: branchOr(req.GetBranch()), Filter: kf, Limit: 1,
	})
	if err != nil {
		return nil, internal(err)
	}
	if len(rows) == 0 {
		return nil, status.Error(codes.NotFound, "no such row")
	}
	return rowToProto(rows[0]), nil
}

// --- Branching ---

func (s *Server) CreateBranch(ctx context.Context, req *pb.CreateBranchRequest) (*pb.RefInfo, error) {
	p, err := s.require(ctx, "branch")
	if err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	from := req.GetFrom()
	if from == "" {
		from = store.DefaultBranch
	}
	r, err := s.store.CreateBranch(ctx, repo, req.GetName(), from, p)
	if err != nil {
		// The §18 depth cap is a deliberate refusal.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.RefInfo{Id: r.ID.String(), Kind: "branch", Name: r.Name,
		Head: r.Head[:], Parent: from, ChainDepth: int32(len(r.Chain))}, nil
}

func (s *Server) Protect(ctx context.Context, req *pb.ProtectRequest) (*pb.RefInfo, error) {
	if _, err := s.require(ctx, "admin"); err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	if err := s.store.SetBranchProtection(ctx, repo, req.GetBranch(),
		store.BranchProtection{Protected: req.GetProtected(),
			MinApprovals: int(req.GetMinApprovals())}); err != nil {
		return nil, internal(err)
	}
	return &pb.RefInfo{Name: req.GetBranch(), Kind: "branch",
		Protected: req.GetProtected(), MinApprovals: req.GetMinApprovals()}, nil
}

// --- Proposals ---

func (s *Server) CreateProposal(ctx context.Context, req *pb.CreateProposalRequest) (*pb.ProposalInfo, error) {
	p, err := s.require(ctx, "write")
	if err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	into := req.GetInto()
	if into == "" {
		into = store.DefaultBranch
	}
	prop, err := s.store.CreateProposal(ctx, repo, req.GetFrom(), into,
		req.GetTitle(), req.GetDescription(), p)
	if err != nil {
		return nil, internal(err)
	}
	return &pb.ProposalInfo{Id: prop.ID, From: req.GetFrom(), Into: into,
		Title: prop.Title, State: prop.State, CreatedBy: p,
		CreatedAt: timestamppb.New(prop.CreatedAt)}, nil
}

func (s *Server) Review(ctx context.Context, req *pb.ReviewRequest) (*pb.Empty, error) {
	// Approving needs its own capability: it is the control that makes branch
	// protection meaningful.
	cap := "approve"
	if req.GetKind() == "comment" {
		cap = "read"
	}
	p, err := s.require(ctx, cap)
	if err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	if err := s.store.AddReview(ctx, repo, req.GetProposalId(),
		req.GetKind(), req.GetBody(), p); err != nil {
		// Self-approval on a protected branch is refused here, not in a client.
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &pb.Empty{}, nil
}

func (s *Server) MergeProposal(ctx context.Context, req *pb.MergeProposalRequest) (*pb.MergeResponse, error) {
	p, err := s.require(ctx, "merge")
	if err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	started := time.Now()
	res, err := s.store.MergeProposal(ctx, repo, t, req.GetProposalId(), p)
	if err != nil {
		var tooLarge *store.ErrMergeTooLarge
		if errors.As(err, &tooLarge) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		// Missing approvals, unresolved objections, multiple merge bases: all
		// deliberate refusals carrying their reason.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	s.metrics.Merge(len(res.Conflicts), time.Since(started))
	out := &pb.MergeResponse{Clean: res.Clean, RowsApplied: int32(res.Applied)}
	if res.Clean {
		out.CommitId = res.Commit[:]
	}
	for _, c := range res.Conflicts {
		out.Conflicts = append(out.Conflicts, &pb.ConflictInfo{
			Pk: []byte(c.PK), Kind: c.Kind.String(),
			Base: c.Base.String(), Ours: c.Ours.String(), Theirs: c.Theirs.String(),
		})
	}
	return out, nil
}

// --- Admin ---

func (s *Server) Purge(ctx context.Context, req *pb.PurgeRequest) (*pb.PurgeReceipt, error) {
	// The `purge` capability is deliberately separate from `admin` (§15.3): a
	// destructive, irreversible operation should not ride along with routine
	// administration.
	p, err := s.require(ctx, "purge")
	if err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	rec, err := s.store.Purge(ctx, repo, t, core.PK(req.GetPk()), req.GetReason(), p)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.PurgeReceipt{
		VersionsRemoved: int32(rec.VersionsRemoved),
		CommitsMarked:   int32(rec.CommitsMarked),
		At:              timestamppb.New(rec.At),
	}, nil
}

func (s *Server) Verify(req *pb.VerifyRequest, srv pb.Admin_VerifyServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return notFound(err)
	}
	branch := branchOr(req.GetBranch())

	if req.GetIntegrity() {
		f := &pb.VerifyFinding{Check: "integrity", Ok: true}
		if err := s.store.VerifyIntegrity(ctx, repo, branch); err != nil {
			f.Ok, f.Detail = false, err.Error()
		}
		if err := srv.Send(f); err != nil {
			return err
		}
	}
	tables, err := s.store.ListTables(ctx, repo)
	if err != nil {
		return internal(err)
	}
	for _, t := range tables {
		if req.GetDrift() {
			f := &pb.VerifyFinding{Check: "drift", Table: t.Physical, Ok: true}
			rep, err := s.store.VerifyDrift(ctx, repo, t)
			if err != nil {
				return internal(err)
			}
			if n := rep.LiveOnly + rep.VersionOnly + rep.Mismatched; n > 0 {
				f.Ok = false
				f.Detail = fmt.Sprintf("%d only live, %d only in history, %d mismatched",
					rep.LiveOnly, rep.VersionOnly, rep.Mismatched)
			}
			if err := srv.Send(f); err != nil {
				return err
			}
		}
		if req.GetIntervals() {
			f := &pb.VerifyFinding{Check: "intervals", Table: t.Physical, Ok: true}
			rep, err := s.store.VerifyIntervals(ctx, t)
			if err != nil {
				return internal(err)
			}
			if len(rep.Overlaps)+len(rep.MultipleOpen) > 0 {
				f.Ok = false
				f.Detail = fmt.Sprintf("%d overlapping interval(s), %d key(s) with multiple open versions",
					len(rep.Overlaps), len(rep.MultipleOpen))
			}
			if err := srv.Send(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- helpers ---

func (s *Server) lookup(ctx context.Context, repoName, table string) (*store.Repo, *store.Table, error) {
	repo, err := s.store.LookupRepo(ctx, repoName)
	if err != nil {
		return nil, nil, notFound(err)
	}
	if table == "" {
		return repo, nil, nil
	}
	t, err := s.store.LoadTable(ctx, repo, table)
	if err != nil {
		return nil, nil, notFound(err)
	}
	return repo, t, nil
}

func branchOr(b string) string {
	if b == "" {
		return store.DefaultBranch
	}
	return b
}

func internal(err error) error { return status.Error(codes.Internal, err.Error()) }
func notFound(err error) error { return status.Error(codes.NotFound, err.Error()) }

func isPrecondition(err error) bool {
	msg := err.Error()
	for _, s := range []string{"moved", "expected head", "FAILED_PRECONDITION"} {
		if contains(msg, s) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func tableInfo(t *store.Table) *pb.TableInfo {
	out := &pb.TableInfo{Id: t.ID, Name: t.Physical, Mode: string(t.Mode), State: t.State}
	for _, c := range t.Columns {
		pk := false
		for _, p := range t.PKColumns {
			if p == c.ID {
				pk = true
			}
		}
		out.Columns = append(out.Columns, &pb.ColumnInfo{
			Id: uint32(c.ID), Name: c.Name, SqlType: c.SQLType,
			Nullable: c.Nullable, PrimaryKey: pk,
		})
	}
	return out
}

func refInfo(r store.Ref) *pb.RefInfo {
	head := r.Head
	return &pb.RefInfo{
		Id: r.ID.String(), Kind: r.Kind, Name: r.Name, Head: head[:],
		HeadSeq: r.HeadSeq, Parent: r.Parent,
		ChainDepth: int32(len(r.Chain)), Protected: r.Protected,
	}
}

// MetadataPrincipal reads a principal from gRPC metadata. Used by the API-key
// authenticator; OIDC replaces the extraction, not the interface.
func MetadataPrincipal(ctx context.Context, key string) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	v := md.Get(key)
	if len(v) == 0 {
		return "", false
	}
	return v[0], true
}
