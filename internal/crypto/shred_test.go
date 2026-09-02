package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func kek(t *testing.T) *LocalEnvelope {
	t.Helper()
	k, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewLocalEnvelope(k)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestDestroyingTheKeyErasesEveryCopy is the whole point (§13.3): one key
// destruction renders every ciphertext for that subject unreadable at once,
// across every commit, branch, backup, and replica, without touching a single
// history row.
func TestDestroyingTheKeyErasesEveryCopy(t *testing.T) {
	dek, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	aad := AAD(1, "customer-42", 3)

	// The same value, encrypted into several historical versions.
	var versions [][]byte
	for i := 0; i < 5; i++ {
		ct, err := Encrypt(dek, []byte("ada@example.com"), aad)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, ct)
	}
	for _, ct := range versions {
		got, err := Decrypt(dek, ct, aad)
		if err != nil || string(got) != "ada@example.com" {
			t.Fatalf("round trip failed: %v", err)
		}
	}

	// Erasure: destroy the key. The ciphertexts are untouched.
	for i := range dek {
		dek[i] = 0
	}
	destroyed := []byte(nil)

	for _, ct := range versions {
		if _, err := Decrypt(destroyed, ct, aad); !errors.Is(err, ErrErased) {
			t.Errorf("after key destruction the value should report ErrErased, got %v", err)
		}
	}
}

// TestNoncesDifferPerCall: the same plaintext must not produce the same
// ciphertext, which is why encrypted columns are not searchable by predicate.
func TestNoncesDifferPerCall(t *testing.T) {
	dek, _ := NewDEK()
	aad := AAD(1, "k", 2)
	a, _ := Encrypt(dek, []byte("same"), aad)
	b, _ := Encrypt(dek, []byte("same"), aad)
	if bytes.Equal(a, b) {
		t.Error("identical plaintexts produced identical ciphertexts: that leaks equality")
	}
}

// TestCiphertextIsBoundToItsCell: moving a ciphertext to another row or column
// must not authenticate, so a value cannot be silently relocated.
func TestCiphertextIsBoundToItsCell(t *testing.T) {
	dek, _ := NewDEK()
	ct, err := Encrypt(dek, []byte("ada@example.com"), AAD(1, "customer-42", 3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(dek, ct, AAD(1, "customer-99", 3)); err == nil {
		t.Error("a ciphertext moved to another row authenticated")
	}
	if _, err := Decrypt(dek, ct, AAD(1, "customer-42", 4)); err == nil {
		t.Error("a ciphertext moved to another column authenticated")
	}
	if _, err := Decrypt(dek, ct, AAD(2, "customer-42", 3)); err == nil {
		t.Error("a ciphertext moved to another table authenticated")
	}
}

// TestEnvelopeWrapsAndUnwraps: the DEK is stored wrapped, so the database never
// holds usable key material.
func TestEnvelopeWrapsAndUnwraps(t *testing.T) {
	e := kek(t)
	dek, _ := NewDEK()
	wrapped, err := e.Wrap(dek)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Error("the wrapped key contains the raw key material")
	}
	got, err := e.Unwrap(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("unwrap did not recover the key")
	}
}

// TestTamperedCiphertextIsRejected.
func TestTamperedCiphertextIsRejected(t *testing.T) {
	dek, _ := NewDEK()
	aad := AAD(1, "k", 2)
	ct, _ := Encrypt(dek, []byte("ada@example.com"), aad)
	ct[len(ct)-1] ^= 0xff
	if _, err := Decrypt(dek, ct, aad); err == nil {
		t.Error("a tampered ciphertext authenticated")
	}
}
