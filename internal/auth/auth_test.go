package auth_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/kyoyu/internal/auth"
	"github.com/ashita-ai/kyoyu/internal/model"
)

func TestHashAndVerifyAPIKey(t *testing.T) {
	hash, err := auth.HashAPIKey("test-key-123")
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	valid, err := auth.VerifyAPIKey("test-key-123", hash)
	require.NoError(t, err)
	assert.True(t, valid)

	valid, err = auth.VerifyAPIKey("wrong-key", hash)
	require.NoError(t, err)
	assert.False(t, valid)
}

func TestJWTIssueAndValidate(t *testing.T) {
	mgr, err := auth.NewJWTManager("", "", 1*time.Hour)
	require.NoError(t, err)

	agent := model.Agent{
		AgentID: "test-agent",
		Name:    "Test",
		Role:    model.RoleAgent,
	}
	agent.ID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	token, expiresAt, err := mgr.IssueToken(agent)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.True(t, expiresAt.After(time.Now()))

	claims, err := mgr.ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, "test-agent", claims.AgentID)
	assert.Equal(t, model.RoleAgent, claims.Role)
}
