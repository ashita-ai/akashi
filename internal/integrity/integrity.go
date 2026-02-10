// Package integrity provides tamper-evident hashing and Merkle tree construction
// for decision audit trails. All functions are pure and deterministic.
package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// ComputeContentHash produces a SHA-256 hex digest from the canonical decision fields.
// The canonical form is: id|decision_type|outcome|confidence|reasoning|valid_from
// where reasoning defaults to empty string if nil, and valid_from is RFC 3339 UTC.
func ComputeContentHash(id uuid.UUID, decisionType, outcome string, confidence float32, reasoning *string, validFrom time.Time) string {
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

// VerifyContentHash recomputes the content hash and compares it to the stored value.
func VerifyContentHash(stored string, id uuid.UUID, decisionType, outcome string, confidence float32, reasoning *string, validFrom time.Time) bool {
	return stored == ComputeContentHash(id, decisionType, outcome, confidence, reasoning, validFrom)
}

// hashPair produces SHA-256(a || b) as a hex string.
func hashPair(a, b string) string {
	sum := sha256.Sum256([]byte(a + b))
	return hex.EncodeToString(sum[:])
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
