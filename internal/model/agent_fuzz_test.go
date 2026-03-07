package model_test

import (
	"strings"
	"testing"

	"github.com/ashita-ai/akashi/internal/model"
)

func FuzzValidateAgentID(f *testing.F) {
	// Seed from existing unit tests.
	f.Add("agent")
	f.Add("test-agent")
	f.Add("agent.v2")
	f.Add("Agent_01")
	f.Add("user@example")
	f.Add("a")
	f.Add(strings.Repeat("a", 255))

	// Invalid seeds.
	f.Add("")
	f.Add(strings.Repeat("a", 256))
	f.Add("has space")
	f.Add("path/agent")
	f.Add("agen\u00e9")
	f.Add("agent\t1")
	f.Add("agent\n1")
	f.Add("agent:1")

	// Injection / adversarial seeds.
	f.Add("'; DROP TABLE agents;--")
	f.Add("<script>alert(1)</script>")
	f.Add("../../../etc/passwd")
	f.Add("\x00null\x00byte")
	f.Add(strings.Repeat("\xff", 300))

	f.Fuzz(func(t *testing.T, id string) {
		err := model.ValidateAgentID(id)

		// Invariant 1: empty → error.
		if len(id) == 0 && err == nil {
			t.Fatal("empty agent_id should be rejected")
		}

		// Invariant 2: >255 bytes → error.
		if len(id) > 255 && err == nil {
			t.Fatal("agent_id over 255 characters should be rejected")
		}

		// Invariant 3: if accepted, every byte must be in the allowed set.
		if err == nil {
			for i := 0; i < len(id); i++ {
				c := id[i]
				if !isAllowedAgentIDChar(c) {
					t.Fatalf("accepted agent_id contains invalid character %q at position %d", c, i)
				}
			}
		}

		// Invariant 4: if all characters are allowed and length is 1-255, it must be accepted.
		if len(id) >= 1 && len(id) <= 255 && allAllowed(id) && err != nil {
			t.Fatalf("agent_id %q should have been accepted but got: %v", id, err)
		}
	})
}

func isAllowedAgentIDChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '.' || c == '-' || c == '_' || c == '@'
}

func allAllowed(id string) bool {
	for i := 0; i < len(id); i++ {
		if !isAllowedAgentIDChar(id[i]) {
			return false
		}
	}
	return true
}
