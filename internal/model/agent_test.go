package model_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestValidateAgentID_Valid(t *testing.T) {
	valid := []string{
		"agent",
		"test-agent",
		"agent.v2",
		"Agent_01",
		"user@example",
		"a",
		strings.Repeat("a", 255),
	}
	for _, id := range valid {
		require.NoError(t, model.ValidateAgentID(id), "expected valid: %q", id)
	}
}

func TestValidateAgentID_Invalid(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty", "", "agent_id is required"},
		{"too long", strings.Repeat("a", 256), "at most 255"},
		{"space", "has space", "invalid character"},
		{"slash", "path/agent", "invalid character"},
		{"unicode", "agen\u00e9", "invalid character"},
		{"tab", "agent\t1", "invalid character"},
		{"newline", "agent\n1", "invalid character"},
		{"colon", "agent:1", "invalid character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := model.ValidateAgentID(tt.id)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
