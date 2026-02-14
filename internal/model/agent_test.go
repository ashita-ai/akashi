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

func TestRoleRank(t *testing.T) {
	// Verify strict ordering: platform_admin > org_owner > admin > agent > reader.
	// Unknown roles must rank below reader.
	tests := []struct {
		role model.AgentRole
		rank int
	}{
		{model.RolePlatformAdmin, 5},
		{model.RoleOrgOwner, 4},
		{model.RoleAdmin, 3},
		{model.RoleAgent, 2},
		{model.RoleReader, 1},
		{model.AgentRole("unknown"), 0},
		{model.AgentRole(""), 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			got := model.RoleRank(tt.role)
			assert.Equal(t, tt.rank, got, "RoleRank(%q)", tt.role)
		})
	}

	// Verify strict ordering between adjacent roles.
	ordered := []model.AgentRole{
		model.RoleReader,
		model.RoleAgent,
		model.RoleAdmin,
		model.RoleOrgOwner,
		model.RolePlatformAdmin,
	}
	for i := 1; i < len(ordered); i++ {
		assert.Greater(t, model.RoleRank(ordered[i]), model.RoleRank(ordered[i-1]),
			"%q should rank higher than %q", ordered[i], ordered[i-1])
	}
}

func TestRoleAtLeast(t *testing.T) {
	tests := []struct {
		name    string
		role    model.AgentRole
		minRole model.AgentRole
		want    bool
	}{
		// Same role: always true.
		{"admin >= admin", model.RoleAdmin, model.RoleAdmin, true},
		{"reader >= reader", model.RoleReader, model.RoleReader, true},
		{"platform_admin >= platform_admin", model.RolePlatformAdmin, model.RolePlatformAdmin, true},

		// Higher role: true.
		{"admin >= agent", model.RoleAdmin, model.RoleAgent, true},
		{"admin >= reader", model.RoleAdmin, model.RoleReader, true},
		{"org_owner >= admin", model.RoleOrgOwner, model.RoleAdmin, true},
		{"platform_admin >= reader", model.RolePlatformAdmin, model.RoleReader, true},

		// Lower role: false.
		{"reader >= admin", model.RoleReader, model.RoleAdmin, false},
		{"agent >= admin", model.RoleAgent, model.RoleAdmin, false},
		{"agent >= org_owner", model.RoleAgent, model.RoleOrgOwner, false},
		{"admin >= platform_admin", model.RoleAdmin, model.RolePlatformAdmin, false},
		{"reader >= platform_admin", model.RoleReader, model.RolePlatformAdmin, false},

		// Unknown roles rank at 0, below reader.
		{"unknown >= reader", model.AgentRole("bogus"), model.RoleReader, false},
		{"reader >= unknown", model.RoleReader, model.AgentRole("bogus"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.RoleAtLeast(tt.role, tt.minRole)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateTag(t *testing.T) {
	t.Run("valid tags", func(t *testing.T) {
		valid := []string{
			"a",
			"abc",
			"my-tag",
			"my_tag",
			"tag123",
			"a-b-c",
			"a1b2c3",
			strings.Repeat("a", 64), // exactly at the limit
		}
		for _, tag := range valid {
			require.NoError(t, model.ValidateTag(tag), "expected valid tag: %q", tag)
		}
	})

	t.Run("invalid tags", func(t *testing.T) {
		tests := []struct {
			name string
			tag  string
			want string // substring expected in error message
		}{
			{"empty", "", "must not be empty"},
			{"too long", strings.Repeat("a", 65), "at most 64"},
			{"starts with digit", "1abc", "must start with a lowercase letter"},
			{"starts with hyphen", "-abc", "must start with a lowercase letter"},
			{"starts with underscore", "_abc", "must start with a lowercase letter"},
			{"starts with uppercase", "Abc", "must start with a lowercase letter"},
			{"contains uppercase", "aBc", "invalid character"},
			{"contains space", "a b", "invalid character"},
			{"contains dot", "a.b", "invalid character"},
			{"contains slash", "a/b", "invalid character"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := model.ValidateTag(tt.tag)
				require.Error(t, err, "expected error for tag %q", tt.tag)
				assert.Contains(t, err.Error(), tt.want)
			})
		}
	})
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
