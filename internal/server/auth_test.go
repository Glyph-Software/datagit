package server

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func withToken(tok string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok))
}

func TestKeysAreStoredHashed(t *testing.T) {
	a := NewAPIKeyAuth()
	a.AddKey("secret-token", "arun@example.com", "write")
	for h := range a.keys {
		if h == "secret-token" {
			t.Fatal("the raw key is stored; a database dump would hand over working credentials")
		}
	}
}

func TestUnknownCredentialIsRejected(t *testing.T) {
	a := NewAPIKeyAuth()
	a.AddKey("good", "arun@example.com", "write")
	if _, err := a.Principal(withToken("bad")); err == nil {
		t.Error("an unknown token authenticated")
	}
	if _, err := a.Principal(context.Background()); err == nil {
		t.Error("a request with no credential authenticated")
	}
}

func TestPurgeIsNotImpliedByAdmin(t *testing.T) {
	a := NewAPIKeyAuth()
	a.AddKey("k", "ops@example.com", "admin")
	ctx := context.Background()
	for _, c := range []string{"read", "write", "branch", "merge", "approve"} {
		if !a.Can(ctx, "ops@example.com", c) {
			t.Errorf("admin should imply %s", c)
		}
	}
	if a.Can(ctx, "ops@example.com", "purge") {
		t.Error("admin implied purge: a destructive, irreversible operation must not " +
			"ride along with routine administration (§15.3)")
	}
}

func TestWriteImpliesRead(t *testing.T) {
	a := NewAPIKeyAuth()
	a.AddKey("k", "w@example.com", "write")
	if !a.Can(context.Background(), "w@example.com", "read") {
		t.Error("write should imply read")
	}
	if a.Can(context.Background(), "w@example.com", "admin") {
		t.Error("write must not imply admin")
	}
}

func TestUnknownPrincipalHoldsNothing(t *testing.T) {
	a := NewAPIKeyAuth()
	for _, c := range Capabilities {
		if a.Can(context.Background(), "nobody@example.com", c) {
			t.Errorf("an unregistered principal holds %s", c)
		}
	}
}
