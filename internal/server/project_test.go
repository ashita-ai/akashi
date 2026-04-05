package server

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRepoNameFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"https with .git", "https://github.com/ashita-ai/akashi.git", "akashi"},
		{"https without .git", "https://github.com/ashita-ai/akashi", "akashi"},
		{"ssh with .git", "git@github.com:ashita-ai/akashi.git", "akashi"},
		{"ssh without .git", "git@github.com:ashita-ai/akashi", "akashi"},
		{"ssh:// URL", "ssh://git@github.com/ashita-ai/akashi.git", "akashi"},
		{"file URL", "file:///home/user/repos/akashi", "akashi"},
		{"plain path", "/home/user/repos/akashi", "akashi"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"trailing slash", "https://github.com/ashita-ai/akashi/", "akashi"},
		{"nested repo", "https://gitlab.com/group/subgroup/repo.git", "repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repoNameFromURL(tt.url)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeTraceProject_ServerInferredOverridesClient(t *testing.T) {
	clientCtx := map[string]any{"project": "dubai"}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "akashi", nil, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	assert.Equal(t, "dubai", clientCtx["project_submitted"])
}

func TestNormalizeTraceProject_ServerMatchesClient(t *testing.T) {
	clientCtx := map[string]any{"project": "akashi"}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "akashi", nil, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	_, hasRaw := clientCtx["project_submitted"]
	assert.False(t, hasRaw, "should not set project_submitted when names match")
}

func TestNormalizeTraceProject_RepoURLOverridesClient(t *testing.T) {
	clientCtx := map[string]any{
		"project":  "cairo-v1",
		"repo_url": "https://github.com/ashita-ai/akashi.git",
	}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "", nil, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	assert.Equal(t, "cairo-v1", clientCtx["project_submitted"])
}

func TestNormalizeTraceProject_AliasLookup(t *testing.T) {
	aliases := map[string]string{
		"bamako":   "akashi",
		"dublin":   "tessera",
		"cairo-v1": "akashi",
	}
	resolveAlias := func(project string) string {
		return aliases[project]
	}

	clientCtx := map[string]any{"project": "bamako"}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "", resolveAlias, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	assert.Equal(t, "bamako", clientCtx["project_submitted"])
}

func TestNormalizeTraceProject_NoChangeWhenCanonical(t *testing.T) {
	clientCtx := map[string]any{"project": "akashi"}
	logger := slog.Default()

	// No server project, no repo_url, alias returns nothing, but project is known.
	resolveAlias := func(_ string) string { return "" }
	projectKnown := func(project string) bool { return project == "akashi" }

	errMsg := normalizeTraceProject(clientCtx, "", resolveAlias, projectKnown, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	_, hasRaw := clientCtx["project_submitted"]
	assert.False(t, hasRaw, "should not modify already-canonical project")
}

func TestNormalizeTraceProject_EmptyClientProject(t *testing.T) {
	clientCtx := map[string]any{}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "", nil, nil, logger)

	assert.Empty(t, errMsg)
	_, hasProject := clientCtx["project"]
	assert.False(t, hasProject, "should not set project when client didn't provide one")
}

func TestNormalizeTraceProject_ServerInferredSetsProject(t *testing.T) {
	// Client didn't send project, server inferred one from roots.
	clientCtx := map[string]any{"model": "claude-opus-4-6"}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "akashi", nil, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	_, hasRaw := clientCtx["project_submitted"]
	assert.False(t, hasRaw, "should not set project_submitted when client had no project")
}

func TestNormalizeTraceProject_ServerTakesPriorityOverRepoURL(t *testing.T) {
	// Both server inference and repo_url available — server wins.
	clientCtx := map[string]any{
		"project":  "dubai",
		"repo_url": "https://github.com/ashita-ai/tessera.git",
	}
	logger := slog.Default()

	errMsg := normalizeTraceProject(clientCtx, "akashi", nil, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "akashi", clientCtx["project"])
	assert.Equal(t, "dubai", clientCtx["project_submitted"])
}

func TestNormalizeTraceProject_RejectsUnknownProject(t *testing.T) {
	clientCtx := map[string]any{"project": "winnipeg-v1"}
	logger := slog.Default()

	resolveAlias := func(_ string) string { return "" }
	projectKnown := func(_ string) bool { return false }

	errMsg := normalizeTraceProject(clientCtx, "", resolveAlias, projectKnown, logger)

	assert.Contains(t, errMsg, "unknown project winnipeg-v1")
	_, hasProject := clientCtx["project"]
	assert.False(t, hasProject, "unknown project should be removed from context")
}

func TestNormalizeTraceProject_NilProjectKnownAcceptsValue(t *testing.T) {
	// When projectKnown is nil (e.g. tests without DB), accept the value.
	clientCtx := map[string]any{"project": "new-project"}
	logger := slog.Default()

	resolveAlias := func(_ string) string { return "" }

	errMsg := normalizeTraceProject(clientCtx, "", resolveAlias, nil, logger)

	assert.Empty(t, errMsg)
	assert.Equal(t, "new-project", clientCtx["project"])
}
