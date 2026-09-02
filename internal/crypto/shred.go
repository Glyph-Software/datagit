// Package crypto implements crypto-shredding: the default erasure path that
// satisfies a right-to-erasure request without breaking the audit trail
// (DESIGN.md §13.3).
//
// The mechanism resolves the GDPR-Article-17-versus-immutable-history tension.
// Each data subject gets a key; personal data is encrypted under it IN THE
// SIDECAR ONLY; erasure destroys the key. Every ciphertext for that subject —
// across every commit, branch, backup, and replica — becomes indistinguishable
// from random bytes at once, without modifying a single history row. Because no
// history row changes, the hash chain stays valid and the audit trail remains
// verifiable.
//
// The live table stays PLAINTEXT. Encrypting it would put DataGit back on the
// read path for exactly the columns applications most need to read, which is the
// one thing the whole design exists to avoid (§2, G2). Erasure therefore has two
// steps: an ordinary commit that deletes or anonymizes the subject's CURRENT
// rows, and the key destruction that renders the HISTORY unreadable.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// KeyLen is the data-encryption-key length (AES-256).
const KeyLen = 32

// ErrErased means the subject's key has been destroyed. Historical reads report
// this rather than a decryption failure, so an erasure is legible as an erasure.
var ErrErased = errors.New("crypto: the data subject's key has been destroyed")

// Envelope wraps a data encryption key under a key-encryption key.
//
// In production the KEK lives in a KMS and never enters this process; the
// interface is kept narrow so that swapping a local KEK for a KMS client does
// not reach into the callers.
type Envelope interface {
	Wrap(dek []byte) ([]byte, error)
	Unwrap(wrapped []byte) ([]byte, error)
}

// NewDEK generates a fresh per-subject key.
func NewDEK() ([]byte, error) {
	k := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, fmt.Errorf("crypto: generating a key: %w", err)
	}
	return k, nil
}

// Encrypt seals a value under a subject's key using AES-GCM.
//
// The nonce is random per call, so the same plaintext encrypts differently every
// time. That is deliberate and has a consequence worth stating: encrypted columns
// are NOT searchable by predicate and cannot be indexed for range or prefix
// search. Equality search would require deterministic encryption, which leaks
// equality — offered only as an explicit per-column opt-in, never as a default
// (§13.3, open question Q7).
func Encrypt(dek, plaintext []byte, aad []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: generating a nonce: %w", err)
	}
	// The nonce is prepended, and the additional authenticated data binds the
	// ciphertext to its row and column: a ciphertext moved to another cell will
	// not authenticate.
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt opens a value. A destroyed key surfaces as ErrErased.
func Decrypt(dek, ciphertext []byte, aad []byte) ([]byte, error) {
	if len(dek) == 0 {
		return nil, ErrErased
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("crypto: ciphertext is too short to contain a nonce")
	}
	nonce, body := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	out, err := gcm.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto: authentication failed: %w", err)
	}
	return out, nil
}

func newGCM(dek []byte) (cipher.AEAD, error) {
	if len(dek) != KeyLen {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeyLen, len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// AAD binds a ciphertext to the cell it belongs to.
func AAD(tableID uint64, pk string, col uint32) []byte {
	return []byte(fmt.Sprintf("datagit/%d/%s/%d", tableID, pk, col))
}

// LocalEnvelope is a KEK held in this process. It exists so the mechanism can be
// tested and demonstrated; production deployments supply a KMS-backed Envelope,
// because losing a DEK is indistinguishable from erasing it and durability is
// therefore the KMS's problem, not DataGit's (§13.3).
type LocalEnvelope struct{ kek []byte }

func NewLocalEnvelope(kek []byte) (*LocalEnvelope, error) {
	if len(kek) != KeyLen {
		return nil, fmt.Errorf("crypto: KEK must be %d bytes", KeyLen)
	}
	return &LocalEnvelope{kek: kek}, nil
}

func (e *LocalEnvelope) Wrap(dek []byte) ([]byte, error) {
	return Encrypt(e.kek, dek, []byte("datagit/dek"))
}

func (e *LocalEnvelope) Unwrap(wrapped []byte) ([]byte, error) {
	return Decrypt(e.kek, wrapped, []byte("datagit/dek"))
}
