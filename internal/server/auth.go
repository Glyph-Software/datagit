package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// APIKeyAuth authenticates by bearer token (§15.2).
//
// Keys are stored HASHED. A database dump therefore does not hand over working
// credentials, and comparison is constant-time so a timing signal does not leak
// the prefix of a valid key.
//
// OIDC replaces this implementation, not the interface: handlers only ever see
// a principal and a capability check.
type APIKeyAuth struct {
	mu sync.RWMutex
	// keyHash -> principal
	keys map[string]string
	// principal -> capabilities
	caps map[string]map[string]bool
}

func NewAPIKeyAuth() *APIKeyAuth {
	return &APIKeyAuth{keys: map[string]string{}, caps: map[string]map[string]bool{}}
}

// HashKey is the at-rest form of an API key.
//
// SHA-256 rather than Argon2id is a deliberate, narrow choice: an API key is
// high-entropy machine-generated material, not a human password, so it is not
// subject to the dictionary attack a slow KDF defends against. §15.2 names
// Argon2id for keys derived from anything a person chose; if that ever becomes
// possible here, this must change with it.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// AddKey registers a key for a principal with a capability set.
func (a *APIKeyAuth) AddKey(key, principal string, capabilities ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys[HashKey(key)] = principal
	if a.caps[principal] == nil {
		a.caps[principal] = map[string]bool{}
	}
	for _, c := range capabilities {
		a.caps[principal][c] = true
	}
}

// AddHashedKey registers a key already in its at-rest form, which is how they
// arrive from the database.
func (a *APIKeyAuth) AddHashedKey(hash, principal string, capabilities ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys[hash] = principal
	if a.caps[principal] == nil {
		a.caps[principal] = map[string]bool{}
	}
	for _, c := range capabilities {
		a.caps[principal][c] = true
	}
}

func (a *APIKeyAuth) Principal(ctx context.Context) (string, error) {
	raw, ok := MetadataPrincipal(ctx, "authorization")
	if !ok {
		return "", fmt.Errorf("no credential presented")
	}
	token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if token == "" {
		return "", fmt.Errorf("no credential presented")
	}
	want := HashKey(token)

	a.mu.RLock()
	defer a.mu.RUnlock()
	// Constant-time comparison against every registered hash, so a timing signal
	// does not reveal which prefix matched.
	var found string
	for h, principal := range a.keys {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			found = principal
		}
	}
	if found == "" {
		return "", fmt.Errorf("credential not recognized")
	}
	return found, nil
}

// Can reports whether a principal holds a capability (§15.3).
//
// The capabilities are exactly DESIGN.md's seven. `purge` is NOT implied by
// `admin`: a destructive, irreversible operation should not ride along with
// routine administration.
func (a *APIKeyAuth) Can(ctx context.Context, principal, capability string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	caps := a.caps[principal]
	if caps == nil {
		return false
	}
	if caps[capability] {
		return true
	}
	// admin implies the ordinary capabilities, but never purge.
	if caps["admin"] {
		switch capability {
		case "read", "write", "branch", "merge", "approve":
			return true
		}
	}
	// write implies read; there is no useful writer who cannot read.
	if caps["write"] && capability == "read" {
		return true
	}
	return false
}

// Capabilities are DESIGN.md §15.3's set.
var Capabilities = []string{"read", "write", "branch", "merge", "approve", "admin", "purge"}
