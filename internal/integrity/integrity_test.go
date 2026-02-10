package integrity

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestComputeContentHash_Deterministic(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	validFrom := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	reasoning := "chose microservices for scalability"

	h1 := ComputeContentHash(id, "architecture", "microservices", 0.85, &reasoning, validFrom)
	h2 := ComputeContentHash(id, "architecture", "microservices", 0.85, &reasoning, validFrom)

	if h1 != h2 {
		t.Fatalf("hash not deterministic: %q != %q", h1, h2)
	}
	if !strings.HasPrefix(h1, "v2:") {
		t.Fatalf("expected v2: prefix, got %q", h1)
	}
	// v2: prefix (3 chars) + 64-char hex SHA-256 = 67 chars total.
	if len(h1) != 67 {
		t.Fatalf("expected 67-char v2 hash (3 prefix + 64 hex), got %d chars", len(h1))
	}
}

func TestComputeContentHash_NilReasoning(t *testing.T) {
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	validFrom := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	h1 := ComputeContentHash(id, "deploy", "production", 0.9, nil, validFrom)
	reasoning := ""
	h2 := ComputeContentHash(id, "deploy", "production", 0.9, &reasoning, validFrom)

	if h1 != h2 {
		t.Fatalf("nil reasoning and empty string reasoning should produce the same hash: %q != %q", h1, h2)
	}
}

func TestComputeContentHash_DifferentInputs(t *testing.T) {
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	validFrom := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	h1 := ComputeContentHash(id, "architecture", "monolith", 0.7, nil, validFrom)
	h2 := ComputeContentHash(id, "architecture", "microservices", 0.7, nil, validFrom)

	if h1 == h2 {
		t.Fatal("different outcomes should produce different hashes")
	}
}

func TestVerifyContentHash_V2(t *testing.T) {
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	validFrom := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	reasoning := "cost analysis favored option B"

	hash := ComputeContentHash(id, "vendor", "option_b", 0.92, &reasoning, validFrom)

	if !VerifyContentHash(hash, id, "vendor", "option_b", 0.92, &reasoning, validFrom) {
		t.Fatal("verification should succeed for matching v2 inputs")
	}

	if VerifyContentHash(hash, id, "vendor", "option_a", 0.92, &reasoning, validFrom) {
		t.Fatal("verification should fail for different outcome")
	}

	if VerifyContentHash("tampered_hash", id, "vendor", "option_b", 0.92, &reasoning, validFrom) {
		t.Fatal("verification should fail for tampered hash")
	}
}

func TestVerifyContentHash_V1Legacy(t *testing.T) {
	// Simulate a legacy v1 hash (pipe-delimited, no version prefix).
	id := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	validFrom := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	reasoning := "legacy decision"

	v1Hash := computeV1Hash(id, "routing", "route_a", 0.8, &reasoning, validFrom)

	// V1 hash should not have a v2 prefix.
	if strings.HasPrefix(v1Hash, "v2:") {
		t.Fatal("v1 hash should not have v2 prefix")
	}
	// Should be a 64-char hex string.
	if len(v1Hash) != 64 {
		t.Fatalf("v1 hash should be 64 hex chars, got %d", len(v1Hash))
	}

	// VerifyContentHash should recognize unprefixed hashes as v1 and verify correctly.
	if !VerifyContentHash(v1Hash, id, "routing", "route_a", 0.8, &reasoning, validFrom) {
		t.Fatal("v1 hash should verify through VerifyContentHash")
	}

	// Wrong data should fail verification.
	if VerifyContentHash(v1Hash, id, "routing", "route_b", 0.8, &reasoning, validFrom) {
		t.Fatal("v1 hash should fail verification for different outcome")
	}
}

func TestV2HashAvoidsPipeCollision(t *testing.T) {
	// Two inputs that would collide with pipe-delimited encoding but not with
	// length-prefixed encoding. In v1, "a|b" + "|" + "c" == "a" + "|" + "b|c"
	// when fields are joined with pipes. V2's length-prefix prevents this.
	id := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	h1 := ComputeContentHash(id, "type|extra", "outcome", 0.5, nil, validFrom)
	h2 := ComputeContentHash(id, "type", "extra|outcome", 0.5, nil, validFrom)

	if h1 == h2 {
		t.Fatal("v2 hashes should not collide when pipe characters appear in different field positions")
	}
}

func TestV2AndV1ProduceDifferentHashes(t *testing.T) {
	id := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	validFrom := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	v1 := computeV1Hash(id, "arch", "mono", 0.9, nil, validFrom)
	v2Full := ComputeContentHash(id, "arch", "mono", 0.9, nil, validFrom)

	// Strip the v2: prefix to compare raw hashes.
	v2Raw := strings.TrimPrefix(v2Full, "v2:")
	if v1 == v2Raw {
		t.Fatal("v1 and v2 raw hashes should differ (different encoding)")
	}
}

func TestBuildMerkleRoot_Empty(t *testing.T) {
	root := BuildMerkleRoot(nil)
	if root != "" {
		t.Fatalf("empty input should produce empty root, got %q", root)
	}
}

func TestBuildMerkleRoot_SingleLeaf(t *testing.T) {
	leaf := "abc123"
	root := BuildMerkleRoot([]string{leaf})
	if root != leaf {
		t.Fatalf("single leaf should be the root: got %q, want %q", root, leaf)
	}
}

func TestBuildMerkleRoot_Deterministic(t *testing.T) {
	leaves := []string{"hash_a", "hash_b", "hash_c", "hash_d"}

	r1 := BuildMerkleRoot(leaves)
	r2 := BuildMerkleRoot(leaves)

	if r1 != r2 {
		t.Fatalf("Merkle root not deterministic: %q != %q", r1, r2)
	}
	if len(r1) != 64 {
		t.Fatalf("expected 64-char hex SHA-256 root, got %d chars", len(r1))
	}
}

func TestBuildMerkleRoot_OrderMatters(t *testing.T) {
	r1 := BuildMerkleRoot([]string{"a", "b", "c"})
	r2 := BuildMerkleRoot([]string{"b", "a", "c"})

	if r1 == r2 {
		t.Fatal("different leaf ordering should produce different roots")
	}
}

func TestBuildMerkleRoot_OddLeafCount(t *testing.T) {
	// With 3 leaves: pair (0,1), promote (2). Then pair (hash01, leaf2) -> root.
	root := BuildMerkleRoot([]string{"x", "y", "z"})
	if root == "" {
		t.Fatal("odd leaf count should still produce a root")
	}
	if len(root) != 64 {
		t.Fatalf("expected 64-char hex SHA-256 root, got %d chars", len(root))
	}
}
