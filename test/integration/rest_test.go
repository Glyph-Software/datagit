package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Glyph-Software/datagit/internal/obs"
	"github.com/Glyph-Software/datagit/internal/server"
	"github.com/Glyph-Software/datagit/internal/store"
)

// restFixture serves the REST surface over an httptest server.
type restFixture struct {
	*fixture
	url string
}

func setupREST(t *testing.T) *restFixture {
	t.Helper()
	f := setup(t)

	auth := server.NewAPIKeyAuth()
	auth.AddKey("arun-key", "arun@example.com", "write", "branch", "merge")
	auth.AddKey("reader-key", "reader@example.com", "read")

	srv := server.New(f.store, auth, obs.New())
	ts := httptest.NewServer(srv.RESTHandler())
	t.Cleanup(ts.Close)
	return &restFixture{fixture: f, url: ts.URL}
}

func (r *restFixture) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, r.url+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// TestRESTEnforcesTheSameAuthorizationAsGRPC is the point of deriving REST from
// the gRPC handlers in process rather than reimplementing it. A policy enforced
// by only one surface is not a policy.
func TestRESTEnforcesTheSameAuthorizationAsGRPC(t *testing.T) {
	r := setupREST(t)
	body := `{"branch":"main","message":"via REST","changes":[]}`

	code, out := r.do(t, "POST", "/v1/repos/catalog/tables/products/commits", "", body)
	if code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated REST commit returned %d, want 401", code)
	}
	if out["message"] == nil {
		t.Error("the error body carries no message; DataGit's refusals explain themselves")
	}

	code, _ = r.do(t, "POST", "/v1/repos/catalog/tables/products/commits", "reader-key", body)
	if code != http.StatusForbidden {
		t.Errorf("a read-only principal writing over REST returned %d, want 403", code)
	}
}

// TestRESTCommitTakesAuthorFromTheCredential: §15.2 holds on this surface too,
// and there is no author field to send.
func TestRESTCommitTakesAuthorFromTheCredential(t *testing.T) {
	r := setupREST(t)
	row := r.row("MUG-01", "Enamel mug", "kitchen", "13.50", "2026-08-14T00:00:00Z")
	cells := map[string]any{}
	for _, c := range row.Cols() {
		v := row[c]
		key := fmt.Sprint(uint32(c))
		switch v.Kind {
		case 5: // text
			cells[key] = map[string]any{"text_value": v.Text}
		case 4: // numeric
			cells[key] = map[string]any{"numeric_value": v.Text}
		case 7: // time
			cells[key] = map[string]any{"time_value": v.AsTime().Format("2006-01-02T15:04:05Z")}
		default:
			cells[key] = map[string]any{"is_null": true}
		}
	}
	body, _ := json.Marshal(map[string]any{
		"branch":  "main",
		"message": "priced via REST",
		"changes": []any{map[string]any{
			"pk":  []byte(r.pk(t, "MUG-01")),
			"op":  "OP_UPDATE",
			"row": map[string]any{"cells": cells},
		}},
	})
	code, out := r.do(t, "POST", "/v1/repos/catalog/tables/products/commits",
		"arun-key", string(body))
	if code != http.StatusOK {
		t.Fatalf("commit over REST returned %d: %v", code, out["message"])
	}

	commits, err := r.store.Log(r.ctx, r.repo, "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	if commits[0].Author != "arun@example.com" {
		t.Errorf("author is %q, want the authenticated principal", commits[0].Author)
	}
	if got := r.livePrice(t, "MUG-01"); got != "13.50" {
		t.Errorf("the REST commit did not reach the live table: %s", got)
	}
}

// TestRESTStreamsBecomeArrays: an HTTP client that asked for a list gets a list.
func TestRESTStreamsBecomeArrays(t *testing.T) {
	r := setupREST(t)
	code, out := r.do(t, "GET", "/v1/repos/catalog/log?branch=main", "reader-key", "")
	if code != http.StatusOK {
		t.Fatalf("log returned %d: %v", code, out["message"])
	}
	items, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("log did not return an items array: %v", out)
	}
	if len(items) == 0 {
		t.Error("log returned no commits")
	}
}

// TestRESTRefusalsCarryTheirReason: DataGit's refusals explain themselves, and
// the REST mapping must not flatten them to a status name.
func TestRESTRefusalsCarryTheirReason(t *testing.T) {
	r := setupREST(t)
	if err := r.store.SetBranchProtection(r.ctx, r.repo, "main",
		store.BranchProtection{Protected: true, MinApprovals: 1}); err != nil {
		t.Fatal(err)
	}
	code, _ := r.do(t, "POST", "/v1/repos/catalog/branches", "arun-key",
		`{"name":"q4","from":"main"}`)
	if code != http.StatusOK {
		t.Fatalf("create branch returned %d", code)
	}
	r.commitOn(t, "q4", "TENT-4P", "outdoor", "268.92")

	code, _ = r.do(t, "POST", "/v1/repos/catalog/proposals", "arun-key",
		`{"from":"q4","into":"main","title":"Q4"}`)
	if code != http.StatusOK {
		t.Fatalf("create proposal returned %d", code)
	}

	code, out := r.do(t, "POST", "/v1/repos/catalog/proposals/1/merge", "arun-key",
		`{"table":"products"}`)
	if code != http.StatusPreconditionFailed {
		t.Errorf("merging without approval returned %d, want 412", code)
	}
	msg, _ := out["message"].(string)
	if msg == "" || !bytes.Contains([]byte(msg), []byte("approval")) {
		t.Errorf("the refusal lost its reason: %q", msg)
	}
}

// TestRESTUnknownRouteIs404.
func TestRESTUnknownRouteIs404(t *testing.T) {
	r := setupREST(t)
	code, out := r.do(t, "GET", "/v1/nope", "reader-key", "")
	if code != http.StatusNotFound {
		t.Errorf("an unknown route returned %d, want 404", code)
	}
	if out["message"] == nil {
		t.Error("the 404 body carries no message")
	}
}
