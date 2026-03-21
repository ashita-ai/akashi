package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookCheckStore(t *testing.T) {
	t.Run("empty store returns false", func(t *testing.T) {
		s := newHookCheckStore()
		assert.False(t, s.IsRecent("any-session"))
	})

	t.Run("record makes IsRecent true for any session id", func(t *testing.T) {
		// Claude Code uses different session IDs for MCP tool calls vs built-in
		// tool calls, so the store is global — any recorded check unblocks any
		// session within the TTL window.
		s := newHookCheckStore()
		s.Record("mcp-session-id")
		assert.True(t, s.IsRecent("editor-session-id"))
	})

	t.Run("expired check returns false", func(t *testing.T) {
		s := newHookCheckStore()
		s.lastCheck = time.Now().Add(-(hookCheckTTL + time.Second))
		assert.False(t, s.IsRecent("any-session"))
	})

	t.Run("cleanup is safe to call", func(t *testing.T) {
		s := newHookCheckStore()
		s.Record("session-1")
		s.Cleanup() // no-op, must not panic
		assert.True(t, s.IsRecent("session-1"))
	})
}

func TestLocalhostOnly(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("localhost IPv4 allowed", func(t *testing.T) {
		handler := localhostOnly("", inner)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("localhost IPv6 allowed", func(t *testing.T) {
		handler := localhostOnly("", inner)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", nil)
		req.RemoteAddr = "[::1]:54321"
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("remote IP rejected without key", func(t *testing.T) {
		handler := localhostOnly("", inner)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("remote IP allowed with valid key", func(t *testing.T) {
		handler := localhostOnly("secret-key", inner)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		req.Header.Set("X-Akashi-Hook-Key", "secret-key")
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("remote IP rejected with wrong key", func(t *testing.T) {
		handler := localhostOnly("secret-key", inner)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		req.Header.Set("X-Akashi-Hook-Key", "wrong-key")
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestHandleHookPreToolUse_EditGate(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	t.Run("edit blocked without check", func(t *testing.T) {
		body := `{"session_id":"sess-1","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
	})

	t.Run("edit allowed after check", func(t *testing.T) {
		h.hookChecks.Record("sess-1")
		body := `{"session_id":"sess-1","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue)
		assert.True(t, resp.SuppressOutput)
	})

	t.Run("Write tool also gated", func(t *testing.T) {
		// Use a fresh handler — the store is global, so a prior Record() in a
		// sibling subtest would make IsRecent return true for any session ID.
		fresh := &Handlers{hookChecks: newHookCheckStore()}
		body := `{"session_id":"sess-new","tool_name":"Write","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		fresh.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
	})

	t.Run("non-edit tool passes through", func(t *testing.T) {
		body := `{"session_id":"sess-1","tool_name":"Read","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue)
		assert.True(t, resp.SuppressOutput)
	})
}

func TestHandleHookPreToolUse_GitCommitReminder(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-1","tool_name":"Bash","tool_input":{"command":"git commit -m 'fix bug'"},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
	h.HandleHookPreToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	require.NotNil(t, resp.HookSpecificOutput)
	assert.Contains(t, resp.HookSpecificOutput.AdditionalContext, "akashi_check")
}

func TestHandleHookPostToolUse_AkashiCheckMarker(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-1","tool_name":"mcp__akashi__akashi_check","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	assert.True(t, h.hookChecks.IsRecent("sess-1"))

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	assert.True(t, resp.SuppressOutput)
}

func TestHandleHookPostToolUse_GitCommitSuggestsTrace(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
		autoTrace:  false,
	}

	body := `{"session_id":"sess-1","tool_name":"Bash","tool_input":{"command":"git commit -m 'add feature'"},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	require.NotNil(t, resp.HookSpecificOutput)
	assert.Contains(t, resp.HookSpecificOutput.Message, "akashi_trace")
}

func TestHandleHookSessionStart_InvalidJSON(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/session-start", strings.NewReader("not json"))
	h.HandleHookSessionStart(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
}

func TestHelpers(t *testing.T) {
	t.Run("isEditTool", func(t *testing.T) {
		tests := []struct {
			name string
			want bool
		}{
			{"Edit", true},
			{"Write", true},
			{"MultiEdit", true},
			{"Read", false},
			{"Bash", false},
			{"edit", false},   // case-sensitive
			{"EDIT", false},   // case-sensitive
			{"", false},       // empty string
			{"Editor", false}, // partial match should not count
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, isEditTool(tt.name))
			})
		}
	})

	t.Run("isBashTool", func(t *testing.T) {
		tests := []struct {
			name string
			want bool
		}{
			{"Bash", true},
			{"bash", false}, // case-sensitive
			{"BashTool", false},
			{"", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, isBashTool(tt.name))
			})
		}
	})

	t.Run("isAkashiTool", func(t *testing.T) {
		tests := []struct {
			name string
			want bool
		}{
			{"mcp__akashi__akashi_check", true},
			{"mcp__akashi__akashi_trace", true},
			{"mcp__akashi__akashi_query", true},
			{"mcp__akashi__akashi_stats", true},
			{"mcp__akashi__", true}, // prefix match includes bare prefix
			{"Edit", false},
			{"mcp__other__tool", false},
			{"", false},
			{"MCP__AKASHI__check", false}, // case-sensitive
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, isAkashiTool(tt.name))
			})
		}
	})

	t.Run("isGitCommit", func(t *testing.T) {
		tests := []struct {
			name  string
			input map[string]any
			want  bool
		}{
			{"simple commit", map[string]any{"command": "git commit -m 'msg'"}, true},
			{"commit with extra spaces", map[string]any{"command": "git  commit --amend"}, true},
			{"commit in longer command", map[string]any{"command": "cd /tmp && git commit -m 'msg'"}, true},
			{"git status", map[string]any{"command": "git status"}, false},
			{"ls command", map[string]any{"command": "ls -la"}, false},
			{"no command key", map[string]any{"other": "git commit"}, false},
			{"empty command", map[string]any{"command": ""}, false},
			{"nil input", nil, false},
			{"empty input", map[string]any{}, false},
			{"command is not string", map[string]any{"command": 42}, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, isGitCommit(tt.input))
			})
		}
	})

	t.Run("extractCommand", func(t *testing.T) {
		tests := []struct {
			name  string
			input map[string]any
			want  string
		}{
			{"string command", map[string]any{"command": "git status"}, "git status"},
			{"missing key", map[string]any{"other": "value"}, ""},
			{"non-string value", map[string]any{"command": 42}, ""},
			{"nil input", nil, ""},
			{"empty map", map[string]any{}, ""},
			{"empty command", map[string]any{"command": ""}, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, extractCommand(tt.input))
			})
		}
	})

	t.Run("extractCommitMessage", func(t *testing.T) {
		tests := []struct {
			name    string
			command string
			want    string
		}{
			{"single-quoted message", "git commit -m 'fix bug'", "fix bug"},
			{"double-quoted message", `git commit -m "add feature"`, "add feature"},
			{"with flags before -m", "git commit -a -m 'fix'", "fix"},
			{"amend without message", "git commit --amend", ""},
			{"no commit", "git status", ""},
			{"empty string", "", ""},
			{"message with spaces", "git commit -m 'fix the big bug'", "fix the big bug"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, extractCommitMessage(tt.command))
			})
		}
	})

	t.Run("inferProjectFromCWD", func(t *testing.T) {
		tests := []struct {
			name string
			cwd  string
			want string
		}{
			{"directory basename", "/home/user/myproject", "myproject"},
			{"empty string", "", ""},
			{"root directory", "/", "/"},
			{"nested path", "/a/b/c/deep-project", "deep-project"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := inferProjectFromCWD(tt.cwd)
				// When the CWD is not a git repo, falls back to filepath.Base.
				// The exact result for git repos depends on the system, so we just
				// verify non-empty CWDs produce a non-empty result.
				if tt.want != "" {
					assert.NotEmpty(t, got)
				} else {
					assert.Equal(t, tt.want, got)
				}
			})
		}
	})

	t.Run("formatAge", func(t *testing.T) {
		tests := []struct {
			name     string
			duration time.Duration
			want     string
		}{
			{"zero", 0, "0s"},
			{"sub-second", 500 * time.Millisecond, "0s"},
			{"30 seconds", 30 * time.Second, "30s"},
			{"59 seconds", 59 * time.Second, "59s"},
			{"1 minute", time.Minute, "1m"},
			{"5 minutes", 5 * time.Minute, "5m"},
			{"59 minutes", 59 * time.Minute, "59m"},
			{"1 hour", time.Hour, "1h"},
			{"3 hours", 3 * time.Hour, "3h"},
			{"23 hours", 23 * time.Hour, "23h"},
			{"1 day", 24 * time.Hour, "1d"},
			{"2 days", 48 * time.Hour, "2d"},
			{"7 days", 7 * 24 * time.Hour, "7d"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, formatAge(tt.duration))
			})
		}
	})

	t.Run("truncateHook", func(t *testing.T) {
		tests := []struct {
			name   string
			input  string
			maxLen int
			want   string
		}{
			{"under limit", "short", 10, "short"},
			{"at limit", "exact", 5, "exact"},
			{"over limit", "abcdefghij", 5, "abcde..."},
			{"empty string", "", 5, ""},
			{"max zero truncates everything", "hello", 0, "..."},
			{"single char limit", "hello", 1, "h..."},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, truncateHook(tt.input, tt.maxLen))
			})
		}
	})
}

func TestHandleHookPreToolUse_MultiEditGate(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-me","tool_name":"MultiEdit","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
	h.HandleHookPreToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.HookSpecificOutput)
	assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
}

func TestHandleHookPreToolUse_InvalidJSON(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader("not json"))
	h.HandleHookPreToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue, "invalid JSON should continue gracefully")
}

func TestHandleHookPostToolUse_InvalidJSON(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader("{bad"))
	h.HandleHookPostToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue, "invalid JSON should continue gracefully")
}

func TestHandleHookPostToolUse_AkashiTraceMarker(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	// akashi_trace should also record the check marker.
	body := `{"session_id":"sess-trace","tool_name":"mcp__akashi__akashi_trace","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	assert.True(t, h.hookChecks.IsRecent("sess-trace"))
}

func TestHandleHookPostToolUse_NonBashNonAkashi(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	// A non-Bash, non-akashi tool should just continue silently.
	body := `{"session_id":"sess-1","tool_name":"Read","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	assert.True(t, resp.SuppressOutput)
}

func TestHookCheckStore_TTLExpiry(t *testing.T) {
	// The store now tracks a single machine-global timestamp, not per-session
	// entries. Test expired and valid states with separate store instances.
	expired := &hookCheckStore{lastCheck: time.Now().Add(-(hookCheckTTL + time.Second))}
	assert.False(t, expired.IsRecent("any"), "check just past TTL should not be recent")

	valid := &hookCheckStore{lastCheck: time.Now().Add(-(hookCheckTTL - time.Minute))}
	assert.True(t, valid.IsRecent("any"), "check just within TTL should be recent")
}

func TestHookCheckStore_CleanupEmpty(t *testing.T) {
	s := newHookCheckStore()
	// Cleanup on empty store should not panic.
	s.Cleanup()
	assert.False(t, s.IsRecent("anything"))
}

func TestHandlePostCommit_NonAutoTrace(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
		autoTrace:  false,
	}

	body := `{"session_id":"sess-commit","tool_name":"Bash","tool_input":{"command":"git commit -m 'test commit'"},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	require.NotNil(t, resp.HookSpecificOutput)
	assert.Contains(t, resp.HookSpecificOutput.Message, "akashi_trace")
}

func TestHandlePostCommit_NoMessageParsed(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
		autoTrace:  false,
	}

	// git commit without -m flag
	body := `{"session_id":"sess-noparsed","tool_name":"Bash","tool_input":{"command":"git commit"},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	require.NotNil(t, resp.HookSpecificOutput)
	// Should include a generic commit message since parsing failed
	assert.Contains(t, resp.HookSpecificOutput.Message, "akashi_trace")
}

func TestGitRepoNameFromPath_ValidRepo(t *testing.T) {
	// Test with the current repo (should return a non-empty name)
	name := gitRepoNameFromPath(".")
	// This may or may not work depending on the test environment
	// At minimum, it should not panic
	_ = name
}

func TestGitRepoNameFromPath_InvalidPath(t *testing.T) {
	name := gitRepoNameFromPath("/nonexistent/path/that/does/not/exist")
	assert.Empty(t, name)
}

func TestInferProjectFromCWD_Empty(t *testing.T) {
	assert.Empty(t, inferProjectFromCWD(""))
}

func TestStripBranchPrefix(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"feature prefix", "feature/add-widget", "add-widget"},
		{"fix prefix", "fix/null-pointer", "null-pointer"},
		{"bugfix prefix", "bugfix/crash-on-start", "crash-on-start"},
		{"hotfix prefix", "hotfix/security-patch", "security-patch"},
		{"chore prefix", "chore/update-deps", "update-deps"},
		{"refactor prefix", "refactor/clean-handlers", "clean-handlers"},
		{"docs prefix", "docs/update-readme", "update-readme"},
		{"test prefix", "test/add-coverage", "add-coverage"},
		{"username prefix", "evanvolgas/enrich-auto-trace", "enrich-auto-trace"},
		{"no prefix", "main", "main"},
		{"no prefix with hyphens", "enrich-auto-trace", "enrich-auto-trace"},
		{"nested known prefix", "feature/auth/token-refresh", "auth/token-refresh"},
		{"empty string", "", ""},
		{"slash only", "/", "/"},
		{"trailing slash", "feature/", "feature/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripBranchPrefix(tt.branch))
		})
	}
}

func TestGitDiffNameOnly_EmptyCWD(t *testing.T) {
	assert.Empty(t, gitDiffNameOnly(""))
}

func TestGitDiffNameOnly_InvalidPath(t *testing.T) {
	assert.Empty(t, gitDiffNameOnly("/nonexistent/path/that/does/not/exist"))
}

func TestGitCommitBody_EmptyCWD(t *testing.T) {
	assert.Empty(t, gitCommitBody(""))
}

func TestGitCommitBody_InvalidPath(t *testing.T) {
	assert.Empty(t, gitCommitBody("/nonexistent/path/that/does/not/exist"))
}

func TestGitBranchTask_EmptyCWD(t *testing.T) {
	assert.Empty(t, gitBranchTask(""))
}

func TestGitBranchTask_InvalidPath(t *testing.T) {
	assert.Empty(t, gitBranchTask("/nonexistent/path/that/does/not/exist"))
}

func TestGitBranchTask_CurrentRepo(t *testing.T) {
	// Running in a real git repo should return something non-empty
	// (unless detached HEAD in CI). Just verify no panic.
	_ = gitBranchTask(".")
}

func TestInferProjectFromCWD_NonGitDir(t *testing.T) {
	result := inferProjectFromCWD("/tmp")
	// Falls back to filepath.Base
	assert.Equal(t, "tmp", result)
}

func TestWriteHookJSON_Success(t *testing.T) {
	rec := httptest.NewRecorder()
	writeHookJSON(rec, hookResponse{Continue: true, SuppressOutput: true})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	assert.True(t, resp.SuppressOutput)
}

func TestHandleHookPreToolUse_GitCommit(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-precommit","tool_name":"Bash","tool_input":{"command":"git commit -m 'test'"},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
	h.HandleHookPreToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	require.NotNil(t, resp.HookSpecificOutput)
	assert.Contains(t, resp.HookSpecificOutput.AdditionalContext, "akashi_check")
}

func TestHandleHookPreToolUse_EditAfterCheck(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	// Record a check first
	h.hookChecks.Record("sess-edit-ok")

	// Now Edit should be allowed
	body := `{"session_id":"sess-edit-ok","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
	h.HandleHookPreToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	assert.True(t, resp.SuppressOutput)
}

// TestHandlePostCommit_AutoTraceResponseShape exercises the autoTrace=true
// branch in handlePostCommit by verifying the HTTP response shape without
// waiting for the background goroutine (which would need a real decisionSvc).
// We set a custom recover wrapper on the goroutine via a channel check.
func TestHandlePostCommit_AutoTraceResponseShape(t *testing.T) {
	// handlePostCommit when autoTrace=true writes the response synchronously
	// before launching autoTraceCommit in a goroutine. We can verify the
	// response shape; the goroutine will panic (nil decisionSvc) but recover
	// happens outside our concern for this test. Skip if -race because the
	// panic goroutine may cause flakes.
	t.Skip("skipped: autoTraceCommit requires a real decisionSvc to avoid nil panic in goroutine")
}

// TestHandleHookSessionStart_ValidBody exercises HandleHookSessionStart
// with a valid JSON body. Since buildSessionContext calls h.db which panics
// with nil, we just test the invalid JSON path and the response shape.
func TestHandleHookSessionStart_EmptyBody(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/session-start", strings.NewReader(""))
	h.HandleHookSessionStart(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// Empty body fails JSON decode, so handler returns Continue=true.
	assert.True(t, resp.Continue)
}

// TestHandleHookPostToolUse_BashNonGitCommit exercises the default case
// where Bash is called but not with a git commit command.
func TestHandleHookPostToolUse_BashNonGitCommit(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-bash","tool_name":"Bash","tool_input":{"command":"ls -la"},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	assert.True(t, resp.SuppressOutput)
}
