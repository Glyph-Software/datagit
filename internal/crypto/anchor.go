package crypto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// Checkpoint is a signed statement of what a branch's head was at a moment
// (§12.3).
//
// This is the mitigation for the honest limit in §12.2: DataGit stores its
// history in the same database as the data, so anyone with direct write access
// can rewrite both the rows and the hashes that attest to them. Writing a signed
// checkpoint to an append-only external store (S3 Object Lock, a WORM bucket, a
// managed ledger) means rewriting history requires compromising TWO systems with
// different access control -- and the checkpoint proves what the head was.
//
// It does not make history tamper-PROOF. It makes a rewrite detectable by
// someone who kept the checkpoints.
type Checkpoint struct {
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	Head      string    `json:"head"`
	Seq       int64     `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	Signature []byte    `json:"signature,omitempty"`
	PublicKey []byte    `json:"public_key,omitempty"`
}

// signingPayload is the exact bytes signed. Deliberately excludes the signature
// and key so that verification is over the same bytes signing was.
func (c Checkpoint) signingPayload() ([]byte, error) {
	bare := Checkpoint{
		Repo: c.Repo, Branch: c.Branch, Head: c.Head,
		Seq: c.Seq, Timestamp: c.Timestamp.UTC(),
	}
	return json.Marshal(bare)
}

// Sign produces a signed checkpoint.
func (c Checkpoint) Sign(priv ed25519.PrivateKey) (Checkpoint, error) {
	payload, err := c.signingPayload()
	if err != nil {
		return c, err
	}
	c.Signature = ed25519.Sign(priv, payload)
	c.PublicKey = priv.Public().(ed25519.PublicKey)
	return c, nil
}

// Verify checks a checkpoint's signature against a trusted key.
//
// The key must be supplied by the caller, not taken from the checkpoint: a
// forger can attach their own key and a matching signature, so trusting the
// embedded key would verify nothing.
func (c Checkpoint) Verify(trusted ed25519.PublicKey) error {
	if len(c.Signature) == 0 {
		return fmt.Errorf("crypto: checkpoint is unsigned")
	}
	payload, err := c.signingPayload()
	if err != nil {
		return err
	}
	if !ed25519.Verify(trusted, payload, c.Signature) {
		return fmt.Errorf("crypto: checkpoint signature does not verify against the trusted key")
	}
	return nil
}

// CommitSignature signs a commit id (§12.3), so a forged commit cannot carry a
// valid signature even if someone can write to the database.
func SignCommit(priv ed25519.PrivateKey, commitID [32]byte) []byte {
	return ed25519.Sign(priv, commitDomain(commitID))
}

func VerifyCommit(pub ed25519.PublicKey, commitID [32]byte, sig []byte) bool {
	return ed25519.Verify(pub, commitDomain(commitID), sig)
}

// commitDomain separates commit signatures from checkpoint signatures, so one
// can never be replayed as the other.
func commitDomain(commitID [32]byte) []byte {
	h := sha256.Sum256(append([]byte("datagit.commit-signature.v1"), commitID[:]...))
	return h[:]
}

// GenerateSigningKey produces an Ed25519 keypair for signing.
func GenerateSigningKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(nil)
}
