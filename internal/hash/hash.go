// Package hash implements DataGit's commit hash chain (DESIGN.md §12.1).
//
// FROZEN as `datagit.commit.v1`. Every function here contributes bytes to commit
// ids that are written into history and never recomputed. Changing any of it
// invalidates every commit hash ever written, so it is settled in M0 rather than
// discovered in M1 (PLAN.md M0.4, workstream W3) and pinned by golden tests.
//
// What this buys, and what it does not: recomputing the chain detects any change
// to a committed row, a commit's metadata, or the parent structure. It is
// tamper-*evidence*, not tamper-proofing — DataGit's history lives in the same
// database as the data, so an operator with write access can rewrite both. See
// §12.2, which says so plainly, and §12.3 for external anchoring.
package hash

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"time"

	"github.com/Glyph-Software/datagit/internal/core"
)

// Size is the digest length in bytes.
const Size = sha256.Size

// Digest is a 32-byte SHA-256 digest.
type Digest [Size]byte

func (d Digest) String() string { return fmt.Sprintf("%x", d[:]) }

func (d Digest) Short() string { return fmt.Sprintf("%x", d[:4]) }

// IsZero reports the all-zero digest, which marks a version staged in a session
// and not yet committed (DESIGN.md §5.2).
func (d Digest) IsZero() bool {
	for _, b := range d {
		if b != 0 {
			return false
		}
	}
	return true
}

// Domain separation tags. Every hash input starts with one, so a digest computed
// for one purpose can never be mistaken for another — without this, a leaf whose
// content happened to equal an internal node's concatenation would collide.
const (
	tagLeaf     byte = 0x00
	tagInternal byte = 0x01
	tagEmpty    byte = 0x02
	tagCommit   byte = 0x03
	tagSchema   byte = 0x04
)

// Change is one row's contribution to a commit's digest.
type Change struct {
	TableID uint64
	PK      core.PK
	Op      core.Op
	Changed core.ColMask
	Row     core.Row // nil for a delete
}

// LeafDigest hashes one row change.
//
// Deletes hash their key and op but not a row image: there is no row image to
// hash, and hashing the pre-image would make the digest depend on state the
// change set does not carry.
func LeafDigest(c Change, cols []core.ColID) (Digest, error) {
	buf := make([]byte, 0, 128)
	buf = append(buf, tagLeaf)
	buf = core.AppendUint64(buf, c.TableID)
	buf = core.AppendLenPrefixed(buf, []byte(c.PK))
	buf = append(buf, byte(c.Op))
	buf = core.AppendLenPrefixed(buf, maskBytes(c.Changed))
	if c.Op != core.OpDelete {
		var err error
		if buf, err = c.Row.Encode(buf, cols); err != nil {
			return Digest{}, fmt.Errorf("hash: leaf for pk %x: %w", c.PK, err)
		}
	}
	return sha256.Sum256(buf), nil
}

// maskBytes renders a column mask canonically: trailing zero words are stripped,
// so masks of different widths over the same columns hash identically. DESIGN.md
// §10.5 lets the mask grow as columns are added, and a commit must not change its
// hash merely because a later schema widened the mask.
func maskBytes(m core.ColMask) []byte {
	n := len(m)
	for n > 0 && m[n-1] == 0 {
		n--
	}
	out := make([]byte, 0, n*8)
	for i := 0; i < n; i++ {
		out = core.AppendUint64(out, m[i])
	}
	return out
}

// MerkleRoot builds the root over a set of leaf digests.
//
// Leaves are sorted, so the root depends on the change *set* and not on the
// order the client happened to send it in. Two clients making the same change
// must produce the same commit id.
//
// An odd node is promoted to the next level rather than duplicated. Duplicating
// it is the well-known CVE-2012-2459 shape, where two different trees yield the
// same root.
func MerkleRoot(leaves []Digest) Digest {
	if len(leaves) == 0 {
		return sha256.Sum256([]byte{tagEmpty})
	}
	level := make([]Digest, len(leaves))
	copy(level, leaves)
	sort.Slice(level, func(i, j int) bool {
		return string(level[i][:]) < string(level[j][:])
	})

	for len(level) > 1 {
		next := make([]Digest, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i]) // promote, never duplicate
				continue
			}
			buf := make([]byte, 0, 1+2*Size)
			buf = append(buf, tagInternal)
			buf = append(buf, level[i][:]...)
			buf = append(buf, level[i+1][:]...)
			next = append(next, sha256.Sum256(buf))
		}
		level = next
	}
	return level[0]
}

// ChangeDigest is the Merkle root over a whole change set.
func ChangeDigest(changes []Change, cols []core.ColID) (Digest, error) {
	leaves := make([]Digest, 0, len(changes))
	for _, c := range changes {
		d, err := LeafDigest(c, cols)
		if err != nil {
			return Digest{}, err
		}
		leaves = append(leaves, d)
	}
	return MerkleRoot(leaves), nil
}

// SchemaColumn is one column of a schema version, for digest purposes.
type SchemaColumn struct {
	ID       core.ColID
	Name     string
	Type     string
	Nullable bool
	PK       bool
}

// SchemaDigest hashes a table's schema version (DESIGN.md §10.1).
//
// Columns are sorted by stable id, so the digest is invariant to declaration
// order and depends only on identity, type, and nullability.
func SchemaDigest(tableID uint64, cols []SchemaColumn) Digest {
	sorted := make([]SchemaColumn, len(cols))
	copy(sorted, cols)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	buf := make([]byte, 0, 128)
	buf = append(buf, tagSchema)
	buf = core.AppendUint64(buf, tableID)
	buf = core.AppendUint32(buf, uint32(len(sorted)))
	for _, c := range sorted {
		buf = core.AppendUint32(buf, uint32(c.ID))
		buf = core.AppendLenPrefixed(buf, []byte(c.Name))
		buf = core.AppendLenPrefixed(buf, []byte(c.Type))
		var flags byte
		if c.Nullable {
			flags |= 1
		}
		if c.PK {
			flags |= 2
		}
		buf = append(buf, flags)
	}
	return sha256.Sum256(buf)
}

// CommitInput is everything that determines a commit's identity.
//
// The author comes from the authenticated principal and never from the client
// (DESIGN.md §15.2), so it is part of the hash: an audit trail whose author can
// be forged without changing the commit id would be decoration.
type CommitInput struct {
	RepoID       [16]byte
	Parents      []Digest
	ChangeDigest Digest
	SchemaDigest Digest
	Author       string
	AuthorAt     time.Time
	Message      string
	ExternalRef  string
}

// CommitID computes a commit's content hash (DESIGN.md §12.1).
//
//	commit_id = SHA-256( "datagit.commit.v1"
//	                   ‖ repo_id ‖ sorted(parent_ids)
//	                   ‖ change_digest ‖ schema_digest
//	                   ‖ author ‖ author_at ‖ message ‖ external_ref )
//
// Parents are sorted so a merge commit's id does not depend on which side was
// recorded first. `author_at` is microseconds since the epoch, matching the
// precision both engines store, so a round trip through the database cannot
// change the hash.
func CommitID(in CommitInput) Digest {
	parents := make([]Digest, len(in.Parents))
	copy(parents, in.Parents)
	sort.Slice(parents, func(i, j int) bool {
		return string(parents[i][:]) < string(parents[j][:])
	})

	buf := make([]byte, 0, 256)
	buf = append(buf, tagCommit)
	buf = core.AppendLenPrefixed(buf, []byte(core.CanonicalVersion))
	buf = append(buf, in.RepoID[:]...)
	buf = core.AppendUint32(buf, uint32(len(parents)))
	for _, p := range parents {
		buf = append(buf, p[:]...)
	}
	buf = append(buf, in.ChangeDigest[:]...)
	buf = append(buf, in.SchemaDigest[:]...)
	buf = core.AppendLenPrefixed(buf, []byte(in.Author))
	buf = core.AppendUint64(buf, uint64(in.AuthorAt.UTC().UnixMicro()))
	buf = core.AppendLenPrefixed(buf, []byte(in.Message))
	buf = core.AppendLenPrefixed(buf, []byte(in.ExternalRef))
	return sha256.Sum256(buf)
}

// VerifyChain recomputes a run of commits and reports the first whose stored id
// does not match its content. This is what `datagit verify --integrity` runs
// (§17.3).
//
// A commit marked `integrity = 'purged'` is expected not to match: a hard purge
// physically removes rows and DataGit records the discontinuity rather than
// re-hashing to hide it (§13.4). Callers pass those ids in `purged`.
func VerifyChain(commits []struct {
	ID    Digest
	Input CommitInput
}, purged map[Digest]bool) error {
	for i, c := range commits {
		if purged[c.ID] {
			continue
		}
		if got := CommitID(c.Input); got != c.ID {
			return fmt.Errorf("hash: commit %d: stored id %s but content hashes to %s",
				i, c.ID.Short(), got.Short())
		}
	}
	return nil
}
