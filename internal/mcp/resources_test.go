package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAgentHistoryURI(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		wantID    string
		wantError bool
		errSubstr string
	}{
		{
			name:   "valid simple agent ID",
			uri:    "akashi://agent/my-agent/history",
			wantID: "my-agent",
		},
		{
			name:   "valid agent ID with @ and hyphen",
			uri:    "akashi://agent/planner@acme-corp/history",
			wantID: "planner@acme-corp",
		},
		{
			name:   "valid agent ID with dots",
			uri:    "akashi://agent/agent.v2/history",
			wantID: "agent.v2",
		},
		{
			name:      "empty agent ID between slashes",
			uri:       "akashi://agent//history",
			wantError: true,
			errSubstr: "empty agent_id",
		},
		{
			name:      "wrong prefix",
			uri:       "other://agent/test/history",
			wantError: true,
			errSubstr: "invalid agent history URI",
		},
		{
			name:      "missing /history suffix",
			uri:       "akashi://agent/test",
			wantError: true,
			errSubstr: "invalid agent history URI",
		},
		{
			name:      "completely invalid URI",
			uri:       "garbage",
			wantError: true,
			errSubstr: "invalid agent history URI",
		},
		{
			name:      "empty string",
			uri:       "",
			wantError: true,
			errSubstr: "invalid agent history URI",
		},
		{
			name:   "agent ID containing history substring",
			uri:    "akashi://agent/test-history-checker/history",
			wantID: "test-history-checker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentID, err := parseAgentHistoryURI(tt.uri)

			if tt.wantError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errSubstr)
				assert.Empty(t, agentID)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantID, agentID)
		})
	}
}
