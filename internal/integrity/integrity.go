// Package integrity provides tamper-evident hashing and Merkle tree construction
// for decision audit trails. All functions are pure and deterministic.
package integrity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Hash version prefixes. New hashes get v2 (length-prefixed encoding).
// Old hashes (no prefix) are treated as v1 (pipe-delimited) for backward compatibility.
const (
	hashV2Prefix = "v2:"
)

// ComputeContentHash produces a versioned SHA-256 hex digest from the canonical decision fields.
// New hashes use the v2 format (length-prefixed binary encoding) and carry a "v2:" prefix.
func ComputeContentHash(id uuid.UUID, decisionType, outcome string, confidence float32, reasoning *string, validFrom time.Time) string {
	return hashV2Prefix + computeV2Hash(id, decisionType, outcome, confidence, reasoning, validFrom)
}

// VerifyContentHash checks whether a stored hash matches the recomputed hash.
// It detects the hash version from the prefix and uses the appropriate algorithm:
//   - "v2:" prefix -> length-prefixed binary encoding (current)
//   - no prefix   -> pipe-delimited encoding (legacy v1)
func VerifyContentHash(stored string, id uuid.UUID, decisionType, outcome string, confidence float32, reasoning *string, validFrom time.Time) bool {
	if strings.HasPrefix(stored, hashV2Prefix) {
		return stored == hashV2Prefix+computeV2Hash(id, decisionType, outcome, confidence, reasoning, validFrom)
	}
	// Legacy v1 hashes (pipe-delimited, no version prefix).
	return stored == computeV1Hash(id, decisionType, outcome, confidence, reasoning, validFrom)
}

// computeV1Hash produces the legacy pipe-delimited SHA-256 hex digest.
// Kept for backward compatibility with hashes created before the v2 format.
func computeV1Hash(id uuid.UUID, decisionType, outcome string, confidence float32, reasoning *string, validFrom time.Time) string {
	r := ""
	if reasoning != nil {
		r = *reasoning
	}
	canonical := fmt.Sprintf("%s|%s|%s|%s|%s|%s",
		id.String(), decisionType, outcome,
		strconv.FormatFloat(float64(confidence), 'f', 10, 32),
		r, validFrom.UTC().Format(time.RFC3339Nano))
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// computeV2Hash produces a length-prefixed SHA-256 hex digest.
// Each field is encoded as a 4-byte big-endian length prefix followed by the field bytes.
// This avoids delimiter collisions when freeform text fields contain pipe characters.
func computeV2Hash(id uuid.UUID, decisionType, outcome string, confidence float32, reasoning *string, validFrom time.Time) string {
	h := sha256.New()
	writeField := func(s string) {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s))) //nolint:gosec // field lengths are bounded by HTTP request body limits (~1MB)
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	writeField(id.String())
	writeField(decisionType)
	writeField(outcome)
	writeField(strconv.FormatFloat(float64(confidence), 'f', 10, 32))
	writeField(validFrom.UTC().Format(time.RFC3339Nano))
	r := ""
	if reasoning != nil {
		r = *reasoning
	}
	writeField(r)
	return hex.EncodeToString(h.Sum(nil))
}

// hashPair produces SHA-256(0x01 || a || b) as a hex string.
// The 0x01 prefix is a domain separator for internal Merkle tree nodes (per RFC 6962),
// ensuring internal node hashes can never collide with leaf content hashes.
func hashPair(a, b string) string {
	h := sha256.New()
	h.Write([]byte{0x01}) // internal node domain separator
	h.Write([]byte(a))
	h.Write([]byte(b))
	return hex.EncodeToString(h.Sum(nil))
}

// BuildMerkleRoot constructs a Merkle tree from leaf hashes and returns the root.
// Leaves must be sorted lexicographically by the caller for determinism.
// If leaves is empty, returns an empty string.
// If leaves has one element, the root is that element.
// Odd-length levels hash the last node with itself for structural binding.
func BuildMerkleRoot(leaves []string) string {
	if len(leaves) == 0 {
		return ""
	}
	if len(leaves) == 1 {
		return leaves[0]
	}

	// Build tree bottom-up.
	level := make([]string, len(leaves))
	copy(level, leaves)

	for len(level) > 1 {
		var next []string
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, hashPair(level[i], level[i+1]))
			} else {
				// Odd node: hash with itself for structural binding to tree position.
				next = append(next, hashPair(level[i], level[i]))
			}
		}
		level = next
	}

	return level[0]
}
