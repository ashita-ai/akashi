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
	store := newHookCheckStore()

	t.Run("empty store returns false", func(t *testing.T) {
		assert.False(t, store.IsRecent("session-1"))
	})

	t.Run("recorded session returns true", func(t *testing.T) {
		store.Record("session-1")
		assert.True(t, store.IsRecent("session-1"))
	})

	t.Run("different session returns false", func(t *testing.T) {
		assert.False(t, store.IsRecent("session-2"))
	})

	t.Run("cleanup removes stale entries", func(t *testing.T) {
		s := &hookCheckStore{
			entries: map[string]time.Time{
				"old":    time.Now().Add(-3 * time.Hour),
				"recent": time.Now().Add(-1 * time.Hour),
			},
		}
		s.Cleanup()
		assert.False(t, s.IsRecent("old"))
		assert.True(t, s.IsRecent("recent"))
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
		body := `{"session_id":"sess-new","tool_name":"Write","tool_input":{},"cwd":"/tmp"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/hooks/pre-tool-use", strings.NewReader(body))
		h.HandleHookPreToolUse(rec, req)

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
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
		assert.True(t, isEditTool("Edit"))
		assert.True(t, isEditTool("Write"))
		assert.True(t, isEditTool("MultiEdit"))
		assert.False(t, isEditTool("Read"))
		assert.False(t, isEditTool("Bash"))
	})

	t.Run("isAkashiTool", func(t *testing.T) {
		assert.True(t, isAkashiTool("mcp__akashi__akashi_check"))
		assert.True(t, isAkashiTool("mcp__akashi__akashi_trace"))
		assert.False(t, isAkashiTool("Edit"))
		assert.False(t, isAkashiTool("mcp__other__tool"))
	})

	t.Run("isGitCommit", func(t *testing.T) {
		assert.True(t, isGitCommit(map[string]any{"command": "git commit -m 'msg'"}))
		assert.True(t, isGitCommit(map[string]any{"command": "git  commit --amend"}))
		assert.False(t, isGitCommit(map[string]any{"command": "git status"}))
		assert.False(t, isGitCommit(map[string]any{"command": "ls -la"}))
	})

	t.Run("extractCommitMessage", func(t *testing.T) {
		assert.Equal(t, "fix bug", extractCommitMessage("git commit -m 'fix bug'"))
		assert.Equal(t, "add feature", extractCommitMessage(`git commit -m "add feature"`))
		assert.Equal(t, "", extractCommitMessage("git commit --amend"))
	})

	t.Run("inferProjectFromCWD", func(t *testing.T) {
		assert.Equal(t, "myproject", inferProjectFromCWD("/home/user/myproject"))
		assert.Equal(t, "", inferProjectFromCWD(""))
	})

	t.Run("formatAge", func(t *testing.T) {
		assert.Equal(t, "30s", formatAge(30*time.Second))
		assert.Equal(t, "5m", formatAge(5*time.Minute))
		assert.Equal(t, "3h", formatAge(3*time.Hour))
		assert.Equal(t, "2d", formatAge(48*time.Hour))
	})

	t.Run("truncateHook", func(t *testing.T) {
		assert.Equal(t, "short", truncateHook("short", 10))
		assert.Equal(t, "abcde...", truncateHook("abcdefghij", 5))
	})
}
