package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTestGitRepo creates a temporary git repository with a fake origin remote.
// Returns the directory path. Skips the test if git is not available.
func makeTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil { //nolint:gosec
		t.Skipf("git unavailable: %s", out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/test/test-repo.git").CombinedOutput(); err != nil { //nolint:gosec
		t.Skipf("git remote add failed: %s", out)
	}
	return dir
}

// makeTestGitRepoOnBranch creates a temporary git repository whose current
// branch is the given name. Uses `git symbolic-ref` on an unborn HEAD so it
// works on any git version without needing an initial commit.
func makeTestGitRepoOnBranch(t *testing.T, branch string) string {
	t.Helper()
	dir := makeTestGitRepo(t)
	if out, err := exec.Command("git", "-C", dir, "symbolic-ref", "HEAD", "refs/heads/"+branch).CombinedOutput(); err != nil { //nolint:gosec
		t.Skipf("git symbolic-ref failed: %s", out)
	}
	return dir
}

func TestHookCheckStore(t *testing.T) {
	t.Run("empty store returns false", func(t *testing.T) {
		s := newHookCheckStore()
		assert.False(t, s.IsRecent("any-agent"))
	})

	t.Run("record makes IsRecent true for same agent", func(t *testing.T) {
		s := newHookCheckStore()
		s.Record("agent-a")
		assert.True(t, s.IsRecent("agent-a"))
	})

	t.Run("record does not unlock different agent", func(t *testing.T) {
		s := newHookCheckStore()
		s.Record("agent-a")
		assert.False(t, s.IsRecent("agent-b"), "agent-b should not be unlocked by agent-a's check")
	})

	t.Run("IsAnyRecent returns true when any agent checked", func(t *testing.T) {
		s := newHookCheckStore()
		assert.False(t, s.IsAnyRecent())
		s.Record("agent-a")
		assert.True(t, s.IsAnyRecent())
	})

	t.Run("expired check returns false", func(t *testing.T) {
		s := newHookCheckStore()
		s.mu.Lock()
		s.checks["old-agent"] = time.Now().Add(-(hookCheckTTL + time.Second))
		s.mu.Unlock()
		assert.False(t, s.IsRecent("old-agent"))
		assert.False(t, s.IsAnyRecent())
	})

	t.Run("cleanup evicts expired entries", func(t *testing.T) {
		s := newHookCheckStore()
		s.Record("fresh-agent")
		s.mu.Lock()
		s.checks["stale-agent"] = time.Now().Add(-(hookCheckTTL + time.Second))
		s.mu.Unlock()
		s.Cleanup()
		s.mu.RLock()
		_, staleExists := s.checks["stale-agent"]
		_, freshExists := s.checks["fresh-agent"]
		s.mu.RUnlock()
		assert.False(t, staleExists, "stale entry should be evicted")
		assert.True(t, freshExists, "fresh entry should survive cleanup")
	})

	t.Run("cleanup on empty store is safe", func(t *testing.T) {
		s := newHookCheckStore()
		s.Cleanup() // must not panic
		assert.False(t, s.IsAnyRecent())
	})

	t.Run("empty agent_id is ignored by Record", func(t *testing.T) {
		// Recording with agent_id="" is a no-op to prevent a single "" entry
		// from satisfying IsAnyRecent for all legacy callers.
		s := newHookCheckStore()
		s.Record("")
		assert.False(t, s.IsRecent(""))
		assert.False(t, s.IsAnyRecent())
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

	t.Run("empty header rejected even when key is configured", func(t *testing.T) {
		// Ensures constant-time compare of empty vs non-empty returns false,
		// not a length-mismatch short-circuit leak.
		handler := localhostOnly("secret-key", inner)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		// No X-Akashi-Hook-Key header set.
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestHandleHookPreToolUse_EditGate(t *testing.T) {
	t.Run("edit blocked without check", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		body := `{"session_id":"sess-1","agent_id":"agent-a","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
	})

	t.Run("edit allowed after check for same agent", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		h.hookChecks.Record("agent-a")
		body := `{"session_id":"sess-1","agent_id":"agent-a","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue)
		assert.True(t, resp.SuppressOutput)
	})

	t.Run("edit blocked for different agent even after another agent's check", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		h.hookChecks.Record("agent-a")
		body := `{"session_id":"sess-2","agent_id":"agent-b","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
	})

	t.Run("edit allowed via fallback when no agent_id in request", func(t *testing.T) {
		// Backwards compatibility: if hook script doesn't send agent_id,
		// falls back to IsAnyRecent.
		h := &Handlers{hookChecks: newHookCheckStore()}
		h.hookChecks.Record("agent-a")
		body := `{"session_id":"sess-1","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue)
		assert.True(t, resp.SuppressOutput)
	})

	t.Run("Write tool also gated per-agent", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		body := `{"session_id":"sess-new","agent_id":"agent-x","tool_name":"Write","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
	})

	t.Run("non-edit tool passes through", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
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

func TestHandleHookPreToolUse_BashPassesThrough(t *testing.T) {
	// Bash is intentionally excluded from the PreToolUse matcher to avoid
	// firing on every shell command. Even if Bash somehow reaches the handler,
	// it should pass through since it's not an edit tool.
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
	assert.True(t, resp.SuppressOutput)
}

func TestHandleHookPostToolUse_AkashiCheckMarker(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-1","agent_id":"agent-a","tool_name":"mcp__akashi__akashi_check","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	assert.True(t, h.hookChecks.IsRecent("agent-a"))
	assert.False(t, h.hookChecks.IsRecent("agent-b"), "unrelated agent should not be marked")

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
			// Non-git directories return empty — no basename fallback.
			{"non-git directory", "/home/user/myproject", ""},
			{"empty string", "", ""},
			{"root directory", "/", ""},
			{"nested non-git path", "/a/b/c/deep-project", ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := inferProjectFromCWD(tt.cwd)
				assert.Equal(t, tt.want, got)
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
			// Rune-safe: CJK characters are 3 bytes each but 1 rune.
			// "日本語テスト" = 6 runes. Truncating at 3 runes should give "日本語..."
			{"CJK rune-safe", "日本語テスト", 3, "日本語..."},
			// Emoji: 🎉 is 4 bytes but 1 rune.
			{"emoji rune-safe", "🎉🎊🎈🎆", 2, "🎉🎊..."},
			// Mixed ASCII and multi-byte.
			{"mixed ascii and CJK", "hi日本", 3, "hi日..."},
			// Under limit with multi-byte — should return original string.
			{"multi-byte under limit", "日本", 5, "日本"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, truncateHook(tt.input, tt.maxLen))
			})
		}
	})
}

func TestHookCheckStore_EmptyProjects(t *testing.T) {
	t.Run("empty string project is ignored", func(t *testing.T) {
		s := newHookCheckStore()
		s.MarkProjectEmpty("")
		assert.False(t, s.IsProjectEmpty(""))
	})

	t.Run("mark and check", func(t *testing.T) {
		s := newHookCheckStore()
		s.MarkProjectEmpty("myproject")
		assert.True(t, s.IsProjectEmpty("myproject"))
		assert.False(t, s.IsProjectEmpty("other"))
	})

	t.Run("clear removes marker", func(t *testing.T) {
		s := newHookCheckStore()
		s.MarkProjectEmpty("myproject")
		s.ClearProjectEmpty("myproject")
		assert.False(t, s.IsProjectEmpty("myproject"))
	})

	t.Run("clear on non-existent project is safe", func(t *testing.T) {
		s := newHookCheckStore()
		s.ClearProjectEmpty("never-marked") // must not panic
		assert.False(t, s.IsProjectEmpty("never-marked"))
	})

	t.Run("clear empty string is safe", func(t *testing.T) {
		s := newHookCheckStore()
		s.ClearProjectEmpty("") // must not panic
	})

	t.Run("cleanup evicts expired empty-project entries", func(t *testing.T) {
		s := newHookCheckStore()
		s.mu.Lock()
		s.emptyProjects["stale"] = time.Now().Add(-hookCheckTTL - time.Second)
		s.emptyProjects["fresh"] = time.Now()
		s.mu.Unlock()

		s.Cleanup()

		assert.False(t, s.IsProjectEmpty("stale"), "expired entry should be evicted")
		assert.True(t, s.IsProjectEmpty("fresh"), "unexpired entry should survive")
	})

	t.Run("expired entry returns false from IsProjectEmpty", func(t *testing.T) {
		s := newHookCheckStore()
		s.mu.Lock()
		s.emptyProjects["old"] = time.Now().Add(-hookCheckTTL - time.Second)
		s.mu.Unlock()

		assert.False(t, s.IsProjectEmpty("old"), "entry past TTL should not count as empty")
	})
}

func TestHandleHookPreToolUse_EmptyProjectBypass(t *testing.T) {
	t.Run("edit allowed for empty project without check", func(t *testing.T) {
		dir := makeTestGitRepo(t)
		h := &Handlers{hookChecks: newHookCheckStore()}
		// Mark the project that the git repo resolves to as empty.
		project := inferProjectFromCWD(dir)
		require.NotEmpty(t, project, "test git repo should resolve to a project name")
		h.hookChecks.MarkProjectEmpty(project)

		body := fmt.Sprintf(`{"session_id":"sess-1","agent_id":"agent-a","tool_name":"Edit","tool_input":{},"cwd":%q}`, dir)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue, "edit should be allowed for empty project")
		assert.True(t, resp.SuppressOutput)
	})

	t.Run("edit still gated for non-empty project", func(t *testing.T) {
		dir := makeTestGitRepo(t)
		h := &Handlers{hookChecks: newHookCheckStore()}
		// Do NOT mark the project as empty — gate should still apply.
		body := fmt.Sprintf(`{"session_id":"sess-1","agent_id":"agent-a","tool_name":"Edit","tool_input":{},"cwd":%q}`, dir)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "deny", resp.HookSpecificOutput.PermissionDecision)
	})
}

func TestHandleHookPostToolUse_TraceClearsEmptyProject(t *testing.T) {
	dir := makeTestGitRepo(t)
	h := &Handlers{hookChecks: newHookCheckStore()}
	project := inferProjectFromCWD(dir)
	require.NotEmpty(t, project, "test git repo should resolve to a project name")
	h.hookChecks.MarkProjectEmpty(project)
	assert.True(t, h.hookChecks.IsProjectEmpty(project), "precondition: project should be empty")

	body := fmt.Sprintf(`{"session_id":"sess-1","agent_id":"agent-a","tool_name":"mcp__akashi__akashi_trace","tool_input":{},"cwd":%q}`, dir)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	assert.False(t, h.hookChecks.IsProjectEmpty(project), "project should no longer be empty after trace")

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
}

func TestHookMaxBytesReader(t *testing.T) {
	// Verify that all three hook handlers reject bodies larger than hookMaxBodyBytes
	// by returning Continue=true (graceful degradation, not an error page).
	oversized := strings.Repeat("x", int(hookMaxBodyBytes)+1)

	t.Run("SessionStart rejects oversized body", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/session-start", strings.NewReader(oversized))
		h.HandleHookSessionStart(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue, "oversized body should fail gracefully with continue=true")
	})

	t.Run("PreToolUse rejects oversized body", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(oversized))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue, "oversized body should fail gracefully with continue=true")
	})

	t.Run("PostToolUse rejects oversized body", func(t *testing.T) {
		h := &Handlers{hookChecks: newHookCheckStore()}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(oversized))
		h.HandleHookPostToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue, "oversized body should fail gracefully with continue=true")
	})
}

func TestHandleHookPreToolUse_MultiEditGate(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	body := `{"session_id":"sess-me","agent_id":"agent-me","tool_name":"MultiEdit","tool_input":{},"cwd":"/tmp"}`
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

	// akashi_trace should also record the check marker for the agent.
	body := `{"session_id":"sess-trace","agent_id":"tracer","tool_name":"mcp__akashi__akashi_trace","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/post-tool-use", strings.NewReader(body))
	h.HandleHookPostToolUse(rec, req)

	assert.True(t, h.hookChecks.IsRecent("tracer"))
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
	t.Run("just past TTL is not recent", func(t *testing.T) {
		s := newHookCheckStore()
		s.mu.Lock()
		s.checks["agent-old"] = time.Now().Add(-(hookCheckTTL + time.Second))
		s.mu.Unlock()
		assert.False(t, s.IsRecent("agent-old"))
	})

	t.Run("just within TTL is recent", func(t *testing.T) {
		s := newHookCheckStore()
		s.mu.Lock()
		s.checks["agent-ok"] = time.Now().Add(-(hookCheckTTL - time.Minute))
		s.mu.Unlock()
		assert.True(t, s.IsRecent("agent-ok"))
	})
}

func TestHookCheckStore_CleanupEmpty(t *testing.T) {
	s := newHookCheckStore()
	// Cleanup on empty store should not panic.
	s.Cleanup()
	assert.False(t, s.IsAnyRecent())
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
		{"feat prefix", "feat/add-widget", "add-widget"},
		{"fix prefix", "fix/null-pointer", "null-pointer"},
		{"bugfix prefix", "bugfix/crash-on-start", "crash-on-start"},
		{"hotfix prefix", "hotfix/security-patch", "security-patch"},
		{"chore prefix", "chore/update-deps", "update-deps"},
		{"refactor prefix", "refactor/clean-handlers", "clean-handlers"},
		{"docs prefix", "docs/update-readme", "update-readme"},
		{"test prefix", "test/add-coverage", "add-coverage"},
		{"username prefix preserved", "evanvolgas/enrich-auto-trace", "evanvolgas/enrich-auto-trace"},
		{"release prefix preserved", "release/v2.1", "release/v2.1"},
		{"dependabot prefix preserved", "dependabot/bump-lodash", "dependabot/bump-lodash"},
		{"ci prefix preserved", "ci/add-linting", "ci/add-linting"},
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

func TestGitCommitSubject_EmptyCWD(t *testing.T) {
	assert.Empty(t, gitCommitSubject(""))
}

func TestGitCommitSubject_InvalidPath(t *testing.T) {
	assert.Empty(t, gitCommitSubject("/nonexistent/path/that/does/not/exist"))
}

func TestGitCommitSubject_CurrentRepo(t *testing.T) {
	// Running in a real git repo should return the latest commit subject.
	subject := gitCommitSubject(".")
	assert.NotEmpty(t, subject, "current repo should have at least one commit")
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

func TestSuggestedTaskLabel(t *testing.T) {
	t.Run("empty cwd", func(t *testing.T) {
		assert.Empty(t, suggestedTaskLabel(""))
	})

	t.Run("non-git path", func(t *testing.T) {
		assert.Empty(t, suggestedTaskLabel("/nonexistent/path/that/does/not/exist"))
	})

	t.Run("suppresses trunk branches", func(t *testing.T) {
		for _, branch := range []string{"main", "master", "develop", "trunk"} {
			t.Run(branch, func(t *testing.T) {
				dir := makeTestGitRepoOnBranch(t, branch)
				assert.Empty(t, suggestedTaskLabel(dir), "trunk branch %q must not surface as a task label", branch)
			})
		}
	})

	t.Run("suppresses trunk branches case-insensitively", func(t *testing.T) {
		dir := makeTestGitRepoOnBranch(t, "MAIN")
		assert.Empty(t, suggestedTaskLabel(dir))
	})

	t.Run("suppresses bare commit-hash names", func(t *testing.T) {
		// 8 hex chars — shape of a short commit hash.
		dir := makeTestGitRepoOnBranch(t, "deadbeef")
		assert.Empty(t, suggestedTaskLabel(dir))
	})

	t.Run("suppresses full-length hash names", func(t *testing.T) {
		dir := makeTestGitRepoOnBranch(t, "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0")
		assert.Empty(t, suggestedTaskLabel(dir))
	})

	t.Run("strips known prefixes", func(t *testing.T) {
		dir := makeTestGitRepoOnBranch(t, "feat/dashboard-period-selector")
		assert.Equal(t, "dashboard-period-selector", suggestedTaskLabel(dir))
	})

	t.Run("passes through username prefixes", func(t *testing.T) {
		dir := makeTestGitRepoOnBranch(t, "evanvolgas/gh-issue-fix")
		assert.Equal(t, "evanvolgas/gh-issue-fix", suggestedTaskLabel(dir))
	})

	t.Run("passes through hyphenated task name without prefix", func(t *testing.T) {
		// Not a trunk branch and not hash-shaped — should pass through.
		dir := makeTestGitRepoOnBranch(t, "add-webhook-retries")
		assert.Equal(t, "add-webhook-retries", suggestedTaskLabel(dir))
	})

	t.Run("name shorter than 7 hex chars is not a hash", func(t *testing.T) {
		// "abc" is all-hex but too short to be a commit hash; treat as a
		// legitimate label rather than stripping it.
		dir := makeTestGitRepoOnBranch(t, "abc")
		assert.Equal(t, "abc", suggestedTaskLabel(dir))
	})
}

func TestInferProjectFromCWD_NonGitDir(t *testing.T) {
	result := inferProjectFromCWD("/tmp")
	// Non-git directories return empty — no basename fallback.
	assert.Empty(t, result)
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

func TestHandleHookPreToolUse_BashGitCommitPassesThrough(t *testing.T) {
	// Even git commit Bash commands should pass through PreToolUse — the
	// pre-commit hint was removed to avoid intercepting every Bash call.
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
	assert.True(t, resp.SuppressOutput)
}

func TestHandleHookPreToolUse_EditAfterCheck(t *testing.T) {
	h := &Handlers{
		hookChecks: newHookCheckStore(),
	}

	// Record a check for the specific agent
	h.hookChecks.Record("agent-edit-ok")

	// Edit should be allowed for that agent
	body := `{"session_id":"sess-edit-ok","agent_id":"agent-edit-ok","tool_name":"Edit","tool_input":{},"cwd":"/tmp"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
	h.HandleHookPreToolUse(rec, req)

	var resp hookResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Continue)
	assert.True(t, resp.SuppressOutput)
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
