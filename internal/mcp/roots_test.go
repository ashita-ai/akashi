package mcp

import (
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
