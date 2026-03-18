package integrity

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func FuzzComputeContentHash(f *testing.F) {
	// Seed with known-good inputs from existing unit tests.
	f.Add(
		"11111111-1111-1111-1111-111111111111",
		"architecture",
		"microservices",
		float32(0.85),
		"chose microservices for scalability",
		true,              // hasReasoning
		int64(1737020400), // 2026-01-16
	)
	f.Add(
		"22222222-2222-2222-2222-222222222222",
		"deploy",
		"production",
		float32(0.9),
		"",
		false, // nil reasoning
		int64(1738368000),
	)
	// Edge: pipe characters in fields (v2 should handle this).
	f.Add(
		"66666666-6666-6666-6666-666666666666",
		"type|extra",
		"outcome",
		float32(0.5),
		"",
		false,
		int64(1748736000),
	)
	// Edge: zero confidence.
	f.Add(
		"33333333-3333-3333-3333-333333333333",
		"trade_off",
		"option_a",
		float32(0.0),
		"",
		true,
		int64(0),
	)
	// Edge: max confidence.
	f.Add(
		"44444444-4444-4444-4444-444444444444",
		"security",
		"deny",
		float32(1.0),
		"absolute certainty",
		true,
		int64(math.MaxInt32),
	)
	// Edge: unicode content.
	f.Add(
		"55555555-5555-5555-5555-555555555555",
		"日本語",
		"決定した",
		float32(0.75),
		"理由は明確です🎯",
		true,
		int64(1700000000),
	)

	f.Fuzz(func(t *testing.T, idStr, decisionType, outcome string, confidence float32, reasoning string, hasReasoning bool, unixSec int64) {
		id, err := uuid.Parse(idStr)
		if err != nil {
			// Use a fixed UUID for invalid UUID strings so we still exercise
			// the hashing logic with the fuzzed text fields.
			id = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		}

		// Skip NaN/Inf — these are not valid confidence values and produce
		// non-deterministic formatting.
		if math.IsNaN(float64(confidence)) || math.IsInf(float64(confidence), 0) {
			return
		}

		validFrom := time.Unix(unixSec, 0).UTC()

		var rPtr *string
		if hasReasoning {
			rPtr = &reasoning
		}

		// Must not panic.
		h1 := ComputeContentHash(id, decisionType, outcome, confidence, rPtr, validFrom)

		// Determinism: same inputs → same hash.
		h2 := ComputeContentHash(id, decisionType, outcome, confidence, rPtr, validFrom)
		if h1 != h2 {
			t.Fatalf("non-deterministic hash: %q != %q", h1, h2)
		}

		// Must have v2 prefix.
		if len(h1) < 3 || h1[:3] != "v2:" {
			t.Fatalf("missing v2: prefix in hash %q", h1)
		}

		// Total length: 3 (prefix) + 64 (hex SHA-256) = 67.
		if len(h1) != 67 {
			t.Fatalf("unexpected hash length %d, want 67", len(h1))
		}

		// Verify round-trips.
		if !VerifyContentHash(h1, id, decisionType, outcome, confidence, rPtr, validFrom) {
			t.Fatal("VerifyContentHash returned false for freshly computed hash")
		}
	})
}

func FuzzBuildMerkleRoot(f *testing.F) {
	f.Add("hash_a\nhash_b\nhash_c\nhash_d")
	f.Add("single_leaf")
	f.Add("")
	f.Add("a\nb\nc")
	f.Add("x\nx") // duplicates

	f.Fuzz(func(t *testing.T, joined string) {
		var leaves []string
		if joined != "" {
			// Split on newlines to get variable-length leaf slices.
			for _, part := range splitOnNewline(joined) {
				if part != "" {
					leaves = append(leaves, part)
				}
			}
		}

		// BuildMerkleRoot requires sorted input; sort the fuzzed leaves.
		slices.Sort(leaves)

		// Must not panic or error on sorted input.
		root, err := BuildMerkleRoot(leaves)
		if err != nil {
			t.Fatalf("BuildMerkleRoot returned unexpected error on sorted input: %v", err)
		}

		if len(leaves) == 0 && root != "" {
			t.Fatal("empty leaves should produce empty root")
		}
		if len(leaves) == 1 && root != leaves[0] {
			t.Fatalf("single leaf should be the root, got %q want %q", root, leaves[0])
		}
		if len(leaves) > 1 {
			if len(root) != 64 {
				t.Fatalf("merkle root should be 64 hex chars, got %d", len(root))
			}
			// Determinism.
			root2, err := BuildMerkleRoot(leaves)
			if err != nil {
				t.Fatalf("BuildMerkleRoot returned error on second call: %v", err)
			}
			if root != root2 {
				t.Fatalf("non-deterministic merkle root")
			}
		}
	})
}

// splitOnNewline splits s on "\n". We avoid importing strings for this
// internal test helper to keep the fuzz corpus self-contained.
func splitOnNewline(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
