// Command datagitd serves DataGit over gRPC (§16, §17.1).
//
// A single stateless binary. All durable state is in the target database, so
// replicas hold nothing authoritative: scale horizontally behind any load
// balancer, no session affinity, no leader election, no local disk.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	pb "github.com/Glyph-Software/datagit/gen/datagit/v1"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/obs"
	"github.com/Glyph-Software/datagit/internal/pg"
	"github.com/Glyph-Software/datagit/internal/server"
	"github.com/Glyph-Software/datagit/internal/store"
)

func main() {
	var (
		dsn     = flag.String("dsn", env("DATAGIT_DSN", ""), "PostgreSQL DSN")
		addr    = flag.String("addr", env("DATAGIT_ADDR", ":8433"), "gRPC listen address")
		admin   = flag.String("admin-addr", env("DATAGIT_ADMIN_ADDR", ":8434"), "health and metrics address")
		keyFile = flag.String("api-keys", env("DATAGIT_API_KEYS", ""), "JSON file of API keys")
	)
	flag.Parse()

	if *dsn == "" {
		fatal("--dsn or $DATAGIT_DSN is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pg.Open(ctx, *dsn)
	if err != nil {
		fatal("connecting: %v", err)
	}
	defer pool.Close()

	st := store.New(pool, postgres.NewWithExec(func(ctx context.Context, sql string) error {
		return pool.Direct().Exec(ctx, sql)
	}))

	// Refuse to run against a control schema newer than this build understands
	// (§17.2). Serving anyway would risk writing history a newer version cannot
	// read back.
	if err := st.CheckControlSchema(ctx); err != nil {
		log.Printf("control schema not ready: %v", err)
		log.Printf("run `datagit repo init <name>` first")
	}

	auth := server.NewAPIKeyAuth()
	if *keyFile != "" {
		if err := loadKeys(auth, *keyFile); err != nil {
			fatal("loading api keys: %v", err)
		}
	} else {
		log.Print("WARNING: no --api-keys supplied; every request will be unauthenticated " +
			"and refused. Commits carry a verified principal (§15.2).")
	}

	metrics := obs.New()
	srv := server.New(st, auth, metrics)

	g := grpc.NewServer()
	pb.RegisterRepositoryServer(g, srv)
	pb.RegisterDataServer(g, srv)
	pb.RegisterVersionServer(g, srv)
	pb.RegisterBranchingServer(g, srv)
	pb.RegisterProposalsServer(g, srv)
	pb.RegisterAdminServer(g, srv)

	hs := health.NewServer()
	healthpb.RegisterHealthServer(g, hs)
	reflection.Register(g)

	// Health and metrics on a separate port, so they can be exposed to a cluster
	// without exposing the data plane.
	go serveAdmin(*admin, pool, metrics)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal("listening on %s: %v", *addr, err)
	}
	log.Printf("datagitd listening on %s (admin on %s)", *addr, *admin)

	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		// Graceful: in-flight commits finish rather than being torn out of their
		// transactions.
		g.GracefulStop()
	}()
	if err := g.Serve(lis); err != nil {
		fatal("serving: %v", err)
	}
}

func serveAdmin(addr string, pool *pg.Pool, m *obs.Metrics) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Readiness is database reachability: without it the service can serve
		// nothing, and a load balancer should route elsewhere.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var one int
		if err := pool.Direct().QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "database unreachable: %v\n", err)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot())
	})
	s := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("admin server: %v", err)
	}
}

type keyEntry struct {
	Key          string   `json:"key"`
	KeyHash      string   `json:"key_hash"`
	Principal    string   `json:"principal"`
	Capabilities []string `json:"capabilities"`
}

func loadKeys(a *server.APIKeyAuth, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var entries []keyEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Principal == "" {
			return fmt.Errorf("an api key entry has no principal")
		}
		switch {
		case e.KeyHash != "":
			a.AddHashedKey(strings.TrimSpace(e.KeyHash), e.Principal, e.Capabilities...)
		case e.Key != "":
			a.AddKey(e.Key, e.Principal, e.Capabilities...)
		default:
			return fmt.Errorf("api key entry for %s has neither key nor key_hash", e.Principal)
		}
	}
	log.Printf("loaded %d api key(s)", len(entries))
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "datagitd: "+format+"\n", args...)
	os.Exit(1)
}
