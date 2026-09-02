package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/Glyph-Software/datagit/gen/datagit/v1"
)

// The REST surface (§16).
//
// gRPC is the source of truth and REST is derived from it. The derivation is
// IN PROCESS: each route calls the same server method a gRPC client would, on
// the same object, so authorization goes through the same `require` call.
//
// That is the property worth having. A gateway that proxies to gRPC gets the
// same result but has to be trusted to; calling the method directly makes "both
// surfaces share the authorization path" a fact about the code rather than a
// claim about the deployment. It also removes a hop, a port, and a second place
// for TLS to be misconfigured.
//
// The cost is that routes are written by hand rather than generated from
// annotations. There are about thirty of them and they change when the proto
// changes, which a test catches.

// route binds a method and path to a handler.
type route struct {
	method string
	// path is a template with {name} segments, e.g. /v1/repos/{repo}/tables.
	path string
	// call decodes the request, invokes the service method, and returns either a
	// single message or a slice of them. A streaming RPC becomes a JSON array:
	// an HTTP client that asked for a list wants a list, not a framed stream it
	// has to reassemble.
	call func(r *http.Request, p map[string]string) (any, error)
}

var marshaler = protojson.MarshalOptions{
	// Field names as written in the proto, so the REST body and the proto file
	// agree and a reader of one can follow the other.
	UseProtoNames: true,
	// Zero values are emitted. Omitting them makes a client guess whether a
	// missing `clean` means false or means the server is older than the field.
	EmitUnpopulated: true,
}

var unmarshaler = protojson.UnmarshalOptions{DiscardUnknown: false}

// RESTHandler returns an http.Handler serving the REST surface.
func (s *Server) RESTHandler() http.Handler {
	routes := s.routes()
	mux := http.NewServeMux()
	// One catch-all: Go's ServeMux patterns cannot express the templates, and a
	// linear match over thirty routes is not the bottleneck next to a database
	// round trip.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if rt.method != r.Method {
				continue
			}
			params, ok := matchPath(rt.path, r.URL.Path)
			if !ok {
				continue
			}
			out, err := rt.call(r, params)
			if err != nil {
				writeError(w, err)
				return
			}
			writeResult(w, out)
			return
		}
		writeError(w, status.Errorf(codes.NotFound,
			"no such route: %s %s", r.Method, r.URL.Path))
	})
	return authMiddleware(mux)
}

// authMiddleware moves the HTTP Authorization header into gRPC metadata, so the
// service methods read the credential the same way on both surfaces.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			ctx := metadata.NewIncomingContext(r.Context(),
				metadata.Pairs("authorization", h))
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// matchPath matches a template against a path, returning the named segments.
func matchPath(template, path string) (map[string]string, bool) {
	tp := strings.Split(strings.Trim(template, "/"), "/")
	pp := strings.Split(strings.Trim(path, "/"), "/")
	if len(tp) != len(pp) {
		return nil, false
	}
	params := map[string]string{}
	for i, seg := range tp {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			params[seg[1:len(seg)-1]] = pp[i]
			continue
		}
		if seg != pp[i] {
			return nil, false
		}
	}
	return params, true
}

func decode(r *http.Request, m proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "reading the body: %v", err)
	}
	if len(body) == 0 {
		return nil
	}
	if err := unmarshaler.Unmarshal(body, m); err != nil {
		// An unknown field is an error rather than ignored: a client that
		// misspelled a field should be told, not silently have it dropped.
		return status.Errorf(codes.InvalidArgument, "malformed request body: %v", err)
	}
	return nil
}

// writeResult renders a single message as an object and a slice as {"items":[…]}.
func writeResult(w http.ResponseWriter, out any) {
	w.Header().Set("Content-Type", "application/json")
	if m, ok := out.(proto.Message); ok {
		b, err := marshaler.Marshal(m)
		if err != nil {
			writeError(w, err)
			return
		}
		_, _ = w.Write(b)
		return
	}
	items, ok := out.([]proto.Message)
	if !ok {
		writeError(w, status.Error(codes.Internal, "unrenderable result"))
		return
	}
	parts := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		b, err := marshaler.Marshal(it)
		if err != nil {
			writeError(w, err)
			return
		}
		parts = append(parts, b)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": parts})
}

// writeError maps a gRPC status to HTTP, preserving the message.
//
// DataGit's refusals explain themselves -- a table with no primary key, a merge
// over the atomic limit, a protected branch with no approvals -- so the message
// is the useful part and is passed through rather than replaced with a generic
// one.
func writeError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code := map[codes.Code]int{
		codes.OK:                 http.StatusOK,
		codes.InvalidArgument:    http.StatusBadRequest,
		codes.NotFound:           http.StatusNotFound,
		codes.AlreadyExists:      http.StatusConflict,
		codes.PermissionDenied:   http.StatusForbidden,
		codes.Unauthenticated:    http.StatusUnauthorized,
		codes.FailedPrecondition: http.StatusPreconditionFailed,
		codes.ResourceExhausted:  http.StatusRequestEntityTooLarge,
		codes.Unimplemented:      http.StatusNotImplemented,
		codes.Unavailable:        http.StatusServiceUnavailable,
	}[st.Code()]
	if code == 0 {
		code = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    st.Code().String(),
		"message": st.Message(),
	})
}

// collector adapts a server-streaming RPC to a slice.
//
// The gRPC stream interfaces are wide, but a handler only ever uses Send and
// Context, so the rest is present to satisfy the interface and does nothing.
type collector struct {
	ctx   context.Context
	items []proto.Message
}

func (c *collector) add(m proto.Message) error {
	c.items = append(c.items, m)
	return nil
}

func (c *collector) Context() context.Context     { return c.ctx }
func (c *collector) SetHeader(metadata.MD) error  { return nil }
func (c *collector) SendHeader(metadata.MD) error { return nil }
func (c *collector) SetTrailer(metadata.MD)       {}
func (c *collector) SendMsg(any) error            { return nil }
func (c *collector) RecvMsg(any) error            { return io.EOF }

// logStream, refsStream and the rest are one-line adapters giving `collector`
// the Send signature each generated stream interface requires.
type logStream struct{ *collector }

func (s logStream) Send(m *pb.CommitInfo) error { return s.add(m) }

type refsStream struct{ *collector }

func (s refsStream) Send(m *pb.RefInfo) error { return s.add(m) }

type conflictStream struct{ *collector }

func (s conflictStream) Send(m *pb.ConflictInfo) error { return s.add(m) }

type verifyStream struct{ *collector }

func (s verifyStream) Send(m *pb.VerifyFinding) error { return s.add(m) }

type rowStream struct{ *collector }

func (s rowStream) Send(m *pb.Row) error { return s.add(m) }

func (s *Server) routes() []route {
	return []route{
		// --- Repository ---
		{"POST", "/v1/repos", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.CreateRepoRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return s.CreateRepo(r.Context(), &req)
		}},
		{"GET", "/v1/repos/{repo}", func(r *http.Request, p map[string]string) (any, error) {
			return s.GetStatus(r.Context(), &pb.GetStatusRequest{Repo: p["repo"]})
		}},
		{"POST", "/v1/repos/{repo}/tables", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.TrackTableRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo = p["repo"]
			return s.TrackTable(r.Context(), &req)
		}},
		{"DELETE", "/v1/repos/{repo}/tables/{table}", func(r *http.Request, p map[string]string) (any, error) {
			return s.UntrackTable(r.Context(), &pb.UntrackTableRequest{
				Repo: p["repo"], Table: p["table"]})
		}},

		// --- Version ---
		{"POST", "/v1/repos/{repo}/tables/{table}/commits", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.CommitRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo, req.Table = p["repo"], p["table"]
			// Note there is no author field to override here, on this surface or
			// any other: it comes from the credential (§15.2).
			return s.Commit(r.Context(), &req)
		}},
		{"GET", "/v1/repos/{repo}/log", func(r *http.Request, p map[string]string) (any, error) {
			c := &collector{ctx: r.Context()}
			if err := s.Log(&pb.LogRequest{
				Repo: p["repo"], Branch: query(r, "branch", "main"),
				Limit: int32(queryInt(r, "limit", 50)),
			}, logStream{c}); err != nil {
				return nil, err
			}
			return c.items, nil
		}},
		{"GET", "/v1/repos/{repo}/refs", func(r *http.Request, p map[string]string) (any, error) {
			c := &collector{ctx: r.Context()}
			if err := s.ListRefs(&pb.ListRefsRequest{Repo: p["repo"]}, refsStream{c}); err != nil {
				return nil, err
			}
			return c.items, nil
		}},
		{"GET", "/v1/repos/{repo}/tables/{table}/rows", func(r *http.Request, p map[string]string) (any, error) {
			c := &collector{ctx: r.Context()}
			if err := s.Scan(&pb.ScanRequest{
				Repo: p["repo"], Table: p["table"],
				Branch: query(r, "branch", "main"),
				Limit:  int32(queryInt(r, "limit", 0)),
			}, rowStream{c}); err != nil {
				return nil, err
			}
			return c.items, nil
		}},
		{"GET", "/v1/repos/{repo}/verify", func(r *http.Request, p map[string]string) (any, error) {
			c := &collector{ctx: r.Context()}
			if err := s.Verify(&pb.VerifyRequest{
				Repo: p["repo"], Branch: query(r, "branch", "main"),
				Drift: true, Integrity: true, Intervals: true,
			}, verifyStream{c}); err != nil {
				return nil, err
			}
			return c.items, nil
		}},
		{"GET", "/v1/repos/{repo}/proposals/{id}/conflicts", func(r *http.Request, p map[string]string) (any, error) {
			c := &collector{ctx: r.Context()}
			if err := s.ListConflicts(&pb.ListConflictsRequest{
				Repo: p["repo"], Table: query(r, "table", ""),
				ProposalId: parseInt64(p["id"]),
			}, conflictStream{c}); err != nil {
				return nil, err
			}
			return c.items, nil
		}},

		// --- Branching ---
		{"POST", "/v1/repos/{repo}/branches", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.CreateBranchRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo = p["repo"]
			return s.CreateBranch(r.Context(), &req)
		}},
		{"DELETE", "/v1/repos/{repo}/branches/{name}", func(r *http.Request, p map[string]string) (any, error) {
			return s.DeleteBranch(r.Context(), &pb.DeleteBranchRequest{
				Repo: p["repo"], Name: p["name"]})
		}},

		// --- Proposals ---
		{"POST", "/v1/repos/{repo}/proposals", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.CreateProposalRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo = p["repo"]
			return s.CreateProposal(r.Context(), &req)
		}},
		{"POST", "/v1/repos/{repo}/proposals/{id}/reviews", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.ReviewRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo, req.ProposalId = p["repo"], parseInt64(p["id"])
			return s.Review(r.Context(), &req)
		}},
		{"POST", "/v1/repos/{repo}/proposals/{id}/merge", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.MergeProposalRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo, req.ProposalId = p["repo"], parseInt64(p["id"])
			return s.MergeProposal(r.Context(), &req)
		}},

		// --- Admin ---
		{"POST", "/v1/repos/{repo}/gc", func(r *http.Request, p map[string]string) (any, error) {
			return s.RunGC(r.Context(), &pb.RunGCRequest{Repo: p["repo"]})
		}},
		{"POST", "/v1/repos/{repo}/prune", func(r *http.Request, p map[string]string) (any, error) {
			var req pb.PruneRequest
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			req.Repo = p["repo"]
			return s.Prune(r.Context(), &req)
		}},
	}
}

func query(r *http.Request, name, def string) string {
	if v := r.URL.Query().Get(name); v != "" {
		return v
	}
	return def
}

func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil {
		return def
	}
	return n
}

func parseInt64(s string) int64 {
	var n int64
	_, _ = fmt.Sscan(s, &n)
	return n
}
