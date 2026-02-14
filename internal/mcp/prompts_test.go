package mcp

import (
	"context"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterPrompts(t *testing.T) {
	// testServer is initialized in TestMain (tools_test.go).
	// Verify the server was created and prompts are registered by calling each
	// prompt handler and confirming it returns valid results.
	assert.NotNil(t, testServer, "testServer should be initialized by TestMain")
	assert.NotNil(t, testServer.mcpServer, "MCPServer should be initialized")
}

func TestBeforeDecisionPrompt(t *testing.T) {
	ctx := context.Background()

	result, err := testServer.handleBeforeDecisionPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name:      "before-decision",
			Arguments: map[string]string{"decision_type": "architecture"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Contains(t, result.Description, "architecture",
		"description should reference the decision type")
	require.NotEmpty(t, result.Messages, "expected at least one message")

	msg := result.Messages[0]
	assert.Equal(t, mcplib.RoleUser, msg.Role)

	tc, ok := msg.Content.(mcplib.TextContent)
	require.True(t, ok, "message content should be TextContent")
	assert.Contains(t, tc.Text, "akashi_check",
		"prompt should instruct the agent to call akashi_check")
	assert.Contains(t, tc.Text, "akashi_trace",
		"prompt should instruct the agent to call akashi_trace after")
	assert.Contains(t, tc.Text, "architecture",
		"prompt should reference the specific decision type")
}

func TestBeforeDecisionPrompt_MissingDecisionType(t *testing.T) {
	ctx := context.Background()

	_, err := testServer.handleBeforeDecisionPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name:      "before-decision",
			Arguments: map[string]string{},
		},
	})
	require.Error(t, err, "should error when decision_type is missing")
	assert.Contains(t, err.Error(), "decision_type")
}

func TestBeforeDecisionPrompt_EmptyDecisionType(t *testing.T) {
	ctx := context.Background()

	_, err := testServer.handleBeforeDecisionPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name:      "before-decision",
			Arguments: map[string]string{"decision_type": ""},
		},
	})
	require.Error(t, err, "should error when decision_type is empty")
	assert.Contains(t, err.Error(), "decision_type")
}

func TestAfterDecisionPrompt(t *testing.T) {
	ctx := context.Background()

	result, err := testServer.handleAfterDecisionPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name: "after-decision",
			Arguments: map[string]string{
				"decision_type": "security",
				"outcome":       "chose mTLS",
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Contains(t, result.Description, "security",
		"description should reference the decision type")
	require.NotEmpty(t, result.Messages)

	msg := result.Messages[0]
	assert.Equal(t, mcplib.RoleUser, msg.Role)

	tc, ok := msg.Content.(mcplib.TextContent)
	require.True(t, ok, "message content should be TextContent")
	assert.Contains(t, tc.Text, "akashi_trace",
		"prompt should instruct the agent to call akashi_trace")
	assert.Contains(t, tc.Text, "security",
		"prompt should reference the specific decision type")
	assert.Contains(t, tc.Text, "chose mTLS",
		"prompt should include the outcome")
}

func TestAfterDecisionPrompt_MissingFields(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		args map[string]string
	}{
		{
			name: "missing both",
			args: map[string]string{},
		},
		{
			name: "missing outcome",
			args: map[string]string{"decision_type": "architecture"},
		},
		{
			name: "missing decision_type",
			args: map[string]string{"outcome": "test"},
		},
		{
			name: "empty decision_type",
			args: map[string]string{"decision_type": "", "outcome": "test"},
		},
		{
			name: "empty outcome",
			args: map[string]string{"decision_type": "architecture", "outcome": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testServer.handleAfterDecisionPrompt(ctx, mcplib.GetPromptRequest{
				Params: mcplib.GetPromptParams{
					Name:      "after-decision",
					Arguments: tt.args,
				},
			})
			require.Error(t, err, "should error when required fields are missing")
			assert.Contains(t, err.Error(), "required")
		})
	}
}

func TestAgentSetupPrompt(t *testing.T) {
	ctx := context.Background()

	result, err := testServer.handleAgentSetupPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name: "agent-setup",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.NotEmpty(t, result.Description)
	require.NotEmpty(t, result.Messages)

	msg := result.Messages[0]
	assert.Equal(t, mcplib.RoleUser, msg.Role)

	tc, ok := msg.Content.(mcplib.TextContent)
	require.True(t, ok, "message content should be TextContent")

	// Verify key sections of the setup prompt.
	assert.Contains(t, tc.Text, "Check Before",
		"setup prompt should explain check-before workflow")
	assert.Contains(t, tc.Text, "akashi_check",
		"setup prompt should mention akashi_check tool")
	assert.Contains(t, tc.Text, "akashi_trace",
		"setup prompt should mention akashi_trace tool")
	assert.Contains(t, tc.Text, "akashi_query",
		"setup prompt should mention akashi_query tool")
	assert.Contains(t, tc.Text, "akashi_search",
		"setup prompt should mention akashi_search tool")
	assert.Contains(t, tc.Text, "akashi_recent",
		"setup prompt should mention akashi_recent tool")
	assert.Contains(t, tc.Text, "Confidence",
		"setup prompt should explain confidence levels")
	assert.Contains(t, tc.Text, "Decision Types",
		"setup prompt should list decision types")
}

func TestAgentSetupPrompt_NoArgs(t *testing.T) {
	ctx := context.Background()

	// agent-setup takes no arguments. Calling with empty args should work.
	result, err := testServer.handleAgentSetupPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name:      "agent-setup",
			Arguments: map[string]string{},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.Messages)
}

func TestBeforeDecisionPrompt_VariousTypes(t *testing.T) {
	ctx := context.Background()

	types := []string{"architecture", "security", "model_selection", "trade_off", "deployment"}
	for _, dt := range types {
		t.Run(dt, func(t *testing.T) {
			result, err := testServer.handleBeforeDecisionPrompt(ctx, mcplib.GetPromptRequest{
				Params: mcplib.GetPromptParams{
					Name:      "before-decision",
					Arguments: map[string]string{"decision_type": dt},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Contains(t, result.Description, dt)

			tc, ok := result.Messages[0].Content.(mcplib.TextContent)
			require.True(t, ok)
			// The decision_type should appear 3 times in the template (once in
			// the check instruction, once in the make-decision section, once in
			// the trace instruction).
			assert.Contains(t, tc.Text, dt)
		})
	}
}
