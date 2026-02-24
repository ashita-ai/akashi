package mcp

import (
	"os/exec"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
)

func TestInferProjectFromRoots(t *testing.T) {
	tests := []struct {
		name  string
		roots []mcplib.Root
		want  string
	}{
		{
			name:  "empty roots",
			roots: nil,
			want:  "",
		},
		{
			name:  "single file URI",
			roots: []mcplib.Root{{URI: "file:///Users/evan/Documents/gh/akashi/akashi"}},
			want:  "akashi",
		},
		{
			name:  "multiple roots uses first",
			roots: []mcplib.Root{{URI: "file:///home/user/project-a"}, {URI: "file:///home/user/project-b"}},
			want:  "project-a",
		},
		{
			name:  "non-file URI skipped",
			roots: []mcplib.Root{{URI: "https://example.com/repo"}, {URI: "file:///home/user/my-project"}},
			want:  "my-project",
		},
		{
			name:  "root path returns empty",
			roots: []mcplib.Root{{URI: "file:///"}},
			want:  "",
		},
		{
			name:  "windows-style path",
			roots: []mcplib.Root{{URI: "file:///C:/Users/dev/my-repo"}},
			want:  "my-repo",
		},
		{
			name:  "trailing slash stripped",
			roots: []mcplib.Root{{URI: "file:///home/user/project/"}},
			want:  "project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferProjectFromRoots(tt.roots)
			assert.Equal(t, tt.want, got)
		})
	}
}

// makeGitRepo creates a temporary git repository with an origin remote for testing.
// Returns the directory path, or skips the test if git is not available.
func makeGitRepo(t *testing.T, originURL string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil { //nolint:gosec
		t.Skipf("git unavailable: %s", out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", originURL).CombinedOutput(); err != nil { //nolint:gosec
		t.Skipf("git remote add failed: %s", out)
	}
	return dir
}

func TestGitRepoName(t *testing.T) {
	t.Run("HTTPS remote extracts repo name", func(t *testing.T) {
		dir := makeGitRepo(t, "https://github.com/example/my-service.git")
		assert.Equal(t, "my-service", gitRepoName(dir))
	})

	t.Run("SSH remote extracts repo name", func(t *testing.T) {
		dir := makeGitRepo(t, "git@github.com:example/my-repo.git")
		assert.Equal(t, "my-repo", gitRepoName(dir))
	})

	t.Run("remote without .git suffix", func(t *testing.T) {
		dir := makeGitRepo(t, "https://github.com/example/no-suffix")
		assert.Equal(t, "no-suffix", gitRepoName(dir))
	})

	t.Run("non-existent path returns empty", func(t *testing.T) {
		assert.Empty(t, gitRepoName("/tmp/no-such-directory-akashi-test-xyz"))
	})

	t.Run("non-git directory returns empty", func(t *testing.T) {
		dir := t.TempDir() // plain dir, no git init
		assert.Empty(t, gitRepoName(dir))
	})
}

func TestInferProjectFromRootsWithGit(t *testing.T) {
	t.Run("empty roots returns empty", func(t *testing.T) {
		assert.Empty(t, inferProjectFromRootsWithGit(nil))
	})

	t.Run("git repo with HTTPS remote uses remote name", func(t *testing.T) {
		dir := makeGitRepo(t, "https://github.com/example/cool-service.git")
		roots := []mcplib.Root{{URI: "file://" + dir}}
		assert.Equal(t, "cool-service", inferProjectFromRootsWithGit(roots))
	})

	t.Run("git repo with SSH remote uses remote name", func(t *testing.T) {
		dir := makeGitRepo(t, "git@github.com:org/ssh-project.git")
		roots := []mcplib.Root{{URI: "file://" + dir}}
		assert.Equal(t, "ssh-project", inferProjectFromRootsWithGit(roots))
	})

	t.Run("non-git path falls back to directory name", func(t *testing.T) {
		roots := []mcplib.Root{{URI: "file:///tmp/my-cool-project"}}
		// /tmp/my-cool-project doesn't exist so git fails; falls back to directory name.
		assert.Equal(t, "my-cool-project", inferProjectFromRootsWithGit(roots))
	})

	t.Run("non-file URI skipped", func(t *testing.T) {
		roots := []mcplib.Root{{URI: "https://example.com/repo"}}
		assert.Empty(t, inferProjectFromRootsWithGit(roots))
	})
}

func TestRootURIs(t *testing.T) {
	tests := []struct {
		name  string
		roots []mcplib.Root
		want  []string
	}{
		{
			name:  "nil roots",
			roots: nil,
			want:  nil,
		},
		{
			name:  "empty roots",
			roots: []mcplib.Root{},
			want:  nil,
		},
		{
			name:  "extracts URIs",
			roots: []mcplib.Root{{URI: "file:///a"}, {URI: "file:///b"}},
			want:  []string{"file:///a", "file:///b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rootURIs(tt.roots)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRootsCache(t *testing.T) {
	cache := newRootsCache()

	// Miss on empty cache.
	_, ok := cache.Get("session-1")
	assert.False(t, ok)

	// Set and get.
	roots := []mcplib.Root{{URI: "file:///test"}}
	cache.Set("session-1", roots)
	got, ok := cache.Get("session-1")
	assert.True(t, ok)
	assert.Equal(t, roots, got)

	// Different session misses.
	_, ok = cache.Get("session-2")
	assert.False(t, ok)

	// Empty slice is cached (distinguishes "checked, no roots" from "not checked").
	cache.Set("session-2", []mcplib.Root{})
	got, ok = cache.Get("session-2")
	assert.True(t, ok)
	assert.Empty(t, got)
}
