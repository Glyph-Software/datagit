package crypto

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func TestCheckpointRoundTrip(t *testing.T) {
	pub, priv, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	c := Checkpoint{Repo: "catalog", Branch: "main", Head: "abc123", Seq: 42,
		Timestamp: time.Now().UTC()}
	signed, err := c.Sign(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.Verify(pub); err != nil {
		t.Errorf("a freshly signed checkpoint failed to verify: %v", err)
	}
}

// TestTamperedCheckpointFails is the point of anchoring: a rewritten history
// cannot produce a checkpoint that verifies against the key the operator kept.
func TestTamperedCheckpointFails(t *testing.T) {
	pub, priv, _ := GenerateSigningKey()
	signed, _ := Checkpoint{Repo: "catalog", Branch: "main", Head: "abc123", Seq: 42,
		Timestamp: time.Now().UTC()}.Sign(priv)

	forged := signed
	forged.Head = "deadbeef"
	if err := forged.Verify(pub); err == nil {
		t.Error("a checkpoint claiming a different head verified")
	}

	rewound := signed
	rewound.Seq = 1
	if err := rewound.Verify(pub); err == nil {
		t.Error("a checkpoint claiming a different sequence verified")
	}
}

// TestEmbeddedKeyIsNotTrusted: a forger can attach their own key and a matching
// signature, so verification must use a key the caller already trusts.
func TestEmbeddedKeyIsNotTrusted(t *testing.T) {
	trustedPub, _, _ := GenerateSigningKey()
	_, forgerPriv, _ := GenerateSigningKey()

	forged, _ := Checkpoint{Repo: "catalog", Branch: "main", Head: "deadbeef", Seq: 1,
		Timestamp: time.Now().UTC()}.Sign(forgerPriv)

	// Self-consistent: it verifies against its OWN embedded key.
	if err := forged.Verify(forged.PublicKey); err != nil {
		t.Fatal("the forgery is not even self-consistent; the test is wrong")
	}
	// And useless: it does not verify against the key the operator kept.
	if err := forged.Verify(trustedPub); err == nil {
		t.Error("a checkpoint signed by an untrusted key verified against the trusted one")
	}
}

func TestUnsignedCheckpointIsRejected(t *testing.T) {
	pub, _, _ := GenerateSigningKey()
	if err := (Checkpoint{Repo: "r", Branch: "main"}).Verify(pub); err == nil {
		t.Error("an unsigned checkpoint verified")
	}
}

func TestCommitSignature(t *testing.T) {
	pub, priv, _ := GenerateSigningKey()
	var id [32]byte
	copy(id[:], "a-commit-id-that-is-32-bytes-ok!")
	sig := SignCommit(priv, id)
	if !VerifyCommit(pub, id, sig) {
		t.Error("a commit signature failed to verify")
	}
	var other [32]byte
	copy(other[:], "a-DIFFERENT-id-that-is-32-bytes!")
	if VerifyCommit(pub, other, sig) {
		t.Error("a commit signature verified against a different commit")
	}
}

// TestCommitSignatureIsNotACheckpointSignature: domain separation, so one
// signature can never be replayed as the other.
func TestCommitSignatureIsNotACheckpointSignature(t *testing.T) {
	_, priv, _ := GenerateSigningKey()
	var id [32]byte
	copy(id[:], "a-commit-id-that-is-32-bytes-ok!")
	commitSig := SignCommit(priv, id)

	c := Checkpoint{Repo: "r", Branch: "main", Head: "x", Seq: 1, Timestamp: time.Now().UTC()}
	payload, _ := c.signingPayload()
	if ed25519.Verify(priv.Public().(ed25519.PublicKey), payload, commitSig) {
		t.Error("a commit signature verified as a checkpoint signature")
	}
}
