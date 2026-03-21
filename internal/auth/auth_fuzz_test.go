package auth_test

import (
	"testing"
	"time"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
)

func FuzzValidateToken(f *testing.F) {
	// Create a manager to issue seed tokens and validate fuzzed ones.
	mgr, err := auth.NewJWTManager("", "", 1*time.Hour)
	if err != nil {
		f.Fatalf("failed to create JWT manager: %v", err)
	}

	// Seed with a valid token.
	agent := model.Agent{
		AgentID: "test-agent",
		Name:    "Test",
		Role:    model.RoleAgent,
	}
	agent.ID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	validToken, _, err := mgr.IssueToken(agent)
	if err != nil {
		f.Fatalf("failed to issue seed token: %v", err)
	}
	f.Add(validToken)

	// Seed with various malformed inputs.
	f.Add("")
	f.Add("not-a-jwt")
	f.Add("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.invalid")
	f.Add("eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.invalid")
	f.Add("a]]]]].......")
	f.Add("eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0NTY3ODkwIn0.") // alg=none attack
	f.Add("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxIn0.AAAA")        // wrong algorithm
	f.Add("\x00\x01\x02\x03")                                 // binary garbage

	f.Fuzz(func(t *testing.T, tokenStr string) {
		if len(tokenStr) > 4096 {
			t.Skip("input too large")
		}
		// Must not panic. Errors are expected for fuzzed inputs.
		_, _ = mgr.ValidateToken(tokenStr)
	})
}
