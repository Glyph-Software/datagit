package server

import (
	"bytes"
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/Glyph-Software/datagit/gen/datagit/v1"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
	"github.com/Glyph-Software/datagit/internal/store"
)

// --- Version ---

func (s *Server) Diff(req *pb.DiffRequest, srv pb.Version_DiffServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return err
	}
	toSeq := req.GetToSeq()
	if toSeq == 0 {
		toSeq = -1 // head
	}
	entries, err := s.store.Diff(ctx, repo, t, branchOr(req.GetBranch()), req.GetFromSeq(), toSeq)
	if err != nil {
		return internal(err)
	}
	for _, e := range entries {
		d := &pb.ChangeDetail{Pk: []byte(e.PK), Op: pb.Op(e.Op)}
		if e.Before != nil {
			d.Before = rowToProto(e.Before)
		}
		if e.After != nil {
			d.After = rowToProto(e.After)
		}
		for _, c := range e.Changed.Cols() {
			d.ChangedColumns = append(d.ChangedColumns, uint32(c))
		}
		if err := srv.Send(d); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) History(req *pb.HistoryRequest, srv pb.Version_HistoryServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return err
	}
	recs, err := s.store.History(ctx, repo, t, branchOr(req.GetBranch()), core.PK(req.GetPk()))
	if err != nil {
		return internal(err)
	}
	for _, r := range recs {
		id := r.CommitID
		v := &pb.RowVersion{
			SeqFrom: r.SeqFrom, SeqTo: r.SeqTo, Op: pb.Op(r.Op), CommitId: id[:],
			Author: r.Author, At: timestamppb.New(r.At), Message: r.Message,
		}
		if r.Row != nil {
			v.Row = rowToProto(r.Row)
		}
		if err := srv.Send(v); err != nil {
			return err
		}
	}
	return nil
}

// --- Branching ---

func (s *Server) DeleteBranch(ctx context.Context, req *pb.DeleteBranchRequest) (*pb.Empty, error) {
	if _, err := s.require(ctx, "branch"); err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	if err := s.store.DeleteBranch(ctx, repo, req.GetName()); err != nil {
		// Refusing to delete a branch with children, or the default branch, is a
		// deliberate decision.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.Empty{}, nil
}

func (s *Server) ListRefs(req *pb.ListRefsRequest, srv pb.Branching_ListRefsServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return notFound(err)
	}
	refs, err := s.store.ListRefs(ctx, repo)
	if err != nil {
		return internal(err)
	}
	for _, r := range refs {
		if err := srv.Send(refInfo(r)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) CreateTag(ctx context.Context, req *pb.CreateTagRequest) (*pb.RefInfo, error) {
	p, err := s.require(ctx, "branch")
	if err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	var d hash.Digest
	copy(d[:], req.GetAtCommit())
	if err := s.store.CreateTag(ctx, repo, req.GetName(), d, p); err != nil {
		return nil, internal(err)
	}
	return &pb.RefInfo{Kind: "tag", Name: req.GetName(), Head: d[:]}, nil
}

func (s *Server) UpdateFromParent(ctx context.Context, req *pb.UpdateFromParentRequest) (*pb.MergeResponse, error) {
	p, err := s.require(ctx, "merge")
	if err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	res, err := s.store.UpdateFromParent(ctx, repo, t, req.GetBranch(), p)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return mergeResponse(res), nil
}

func (s *Server) Materialize(ctx context.Context, req *pb.MaterializeRequest) (*pb.Empty, error) {
	if _, err := s.require(ctx, "read"); err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	if err := s.store.Materialize(ctx, repo, req.GetBranch(), req.GetIntoSchema()); err != nil {
		return nil, internal(err)
	}
	return &pb.Empty{}, nil
}

// --- Sessions ---

func (s *Server) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.SessionInfo, error) {
	p, err := s.require(ctx, "write")
	if err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	sess, err := s.store.OpenSession(ctx, repo, req.GetBranch(), p)
	if err != nil {
		// A session on the default branch is refused: there is no uncommitted
		// state there (§6.1).
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	s.metrics.SessionOpened()
	return &pb.SessionInfo{
		Id: sess.ID.String(), Branch: sess.Branch, BaseCommit: sess.Base[:],
		LeaseUntil: timestamppb.New(sess.LeaseUntil),
	}, nil
}

func (s *Server) SessionWrite(ctx context.Context, req *pb.SessionWriteRequest) (*pb.Empty, error) {
	if _, err := s.require(ctx, "write"); err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	sid, err := parseUUID(req.GetSessionId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	changes, err := changesFromProto(req.GetChanges())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.store.SessionWrite(ctx, repo, t, sid, changes); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.Empty{}, nil
}

func (s *Server) CommitSession(ctx context.Context, req *pb.CommitSessionRequest) (*pb.CommitResponse, error) {
	p, err := s.require(ctx, "write")
	if err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	sid, err := parseUUID(req.GetSessionId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	res, err := s.store.CommitSession(ctx, repo, t, sid, p, req.GetMessage())
	if err != nil {
		// A moved branch is the caller's to re-open against the new head.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	s.metrics.SessionClosed()
	return &pb.CommitResponse{Id: res.ID[:], Seq: res.Seq, RowsChanged: int32(res.Changed)}, nil
}

func (s *Server) AbandonSession(ctx context.Context, req *pb.AbandonSessionRequest) (*pb.Empty, error) {
	if _, err := s.require(ctx, "write"); err != nil {
		return nil, err
	}
	_, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	sid, err := parseUUID(req.GetSessionId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.store.AbandonSession(ctx, t, sid); err != nil {
		return nil, internal(err)
	}
	s.metrics.SessionClosed()
	return &pb.Empty{}, nil
}

// --- Proposals ---

func (s *Server) ListConflicts(req *pb.ListConflictsRequest, srv pb.Proposals_ListConflictsServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	_, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return err
	}
	cs, err := s.store.ListConflicts(ctx, t, req.GetProposalId())
	if err != nil {
		return internal(err)
	}
	for _, c := range cs {
		if err := srv.Send(&pb.ConflictInfo{
			Id: c.ID, Pk: []byte(c.PK), Column: c.Column, Kind: c.Kind,
			Base: c.Base, Ours: c.Ours, Theirs: c.Theirs, Resolved: c.Resolved,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ResolveConflict(ctx context.Context, req *pb.ResolveConflictRequest) (*pb.Empty, error) {
	p, err := s.require(ctx, "write")
	if err != nil {
		return nil, err
	}
	if err := s.store.ResolveConflict(ctx, req.GetConflictId(),
		req.GetResolution(), req.GetValue(), p); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.Empty{}, nil
}

// --- Admin ---

func (s *Server) Prune(ctx context.Context, req *pb.PruneRequest) (*pb.PruneResponse, error) {
	if _, err := s.require(ctx, "admin"); err != nil {
		return nil, err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return nil, err
	}
	rep, err := s.store.Prune(ctx, repo, t, store.RetentionPolicy{
		KeepDays: int(req.GetKeepDays()), KeepCommits: int(req.GetKeepCommits()),
	})
	if err != nil {
		return nil, internal(err)
	}
	return &pb.PruneResponse{
		VersionsRemoved: int32(rep.VersionsRemoved), CommitsProtected: int32(rep.CommitsProtected),
	}, nil
}

func (s *Server) RunGC(ctx context.Context, req *pb.RunGCRequest) (*pb.GCResponse, error) {
	if _, err := s.require(ctx, "admin"); err != nil {
		return nil, err
	}
	repo, err := s.store.LookupRepo(ctx, req.GetRepo())
	if err != nil {
		return nil, notFound(err)
	}
	rep, err := s.store.GC(ctx, repo)
	if err != nil {
		return nil, internal(err)
	}
	return &pb.GCResponse{
		OrphanVersions: int32(rep.OrphanVersions), SessionsReaped: int32(rep.SessionsReaped),
	}, nil
}

func (s *Server) Export(req *pb.ExportRequest, srv pb.Admin_ExportServer) error {
	ctx := srv.Context()
	if _, err := s.require(ctx, "read"); err != nil {
		return err
	}
	repo, t, err := s.lookup(ctx, req.GetRepo(), req.GetTable())
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := s.store.Export(ctx, repo, t, branchOr(req.GetBranch()), &buf); err != nil {
		return internal(err)
	}
	// Chunked so a large history streams rather than materializing in one frame.
	const chunk = 64 << 10
	b := buf.Bytes()
	for len(b) > 0 {
		n := chunk
		if n > len(b) {
			n = len(b)
		}
		if err := srv.Send(&pb.ExportChunk{Jsonl: b[:n]}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func mergeResponse(res *store.MergeResult) *pb.MergeResponse {
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
	return out
}
