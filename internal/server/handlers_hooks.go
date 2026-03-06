package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/storage"
)

// hookCheckStore tracks when each session last called akashi_check.
// Replaces the file-based /tmp marker approach with an in-memory store
// that works reliably across platforms.
type hookCheckStore struct {
	mu      sync.RWMutex
	entries map[string]time.Time // session_id -> last check time
}

const hookCheckTTL = 2 * time.Hour

func newHookCheckStore() *hookCheckStore {
	return &hookCheckStore{
		entries: make(map[string]time.Time),
	}
}

func (s *hookCheckStore) Record(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sessionID] = time.Now()
}

func (s *hookCheckStore) IsRecent(sessionID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.entries[sessionID]
	return ok && time.Since(t) < hookCheckTTL
}

// Cleanup removes entries older than the TTL. Called periodically.
func (s *hookCheckStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-hookCheckTTL)
	for k, v := range s.entries {
		if v.Before(cutoff) {
			delete(s.entries, k)
		}
	}
}

// hookSessionStartInput is the JSON body sent by Claude Code / Cursor on SessionStart.
type hookSessionStartInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Source    string `json:"source"` // "startup", "resume", etc.
	Model     string `json:"model"`
}

// hookPreToolUseInput is the JSON body sent on PreToolUse events.
type hookPreToolUseInput struct {
	SessionID     string         `json:"session_id"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	HookEventName string         `json:"hook_event_name"`
	CWD           string         `json:"cwd"`
}

// hookPostToolUseInput is the JSON body sent on PostToolUse events.
type hookPostToolUseInput struct {
	SessionID     string         `json:"session_id"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolResponse  string         `json:"tool_response"`
	HookEventName string         `json:"hook_event_name"`
	CWD           string         `json:"cwd"`
}

// hookResponse is the JSON output format expected by Claude Code and Cursor hooks.
type hookResponse struct {
	Continue           bool          `json:"continue,omitempty"`
	SuppressOutput     bool          `json:"suppressOutput,omitempty"`
	HookSpecificOutput *hookSpecific `json:"hookSpecificOutput,omitempty"`
	Decision           string        `json:"decision,omitempty"`
	Reason             string        `json:"reason,omitempty"`
	SystemMessage      string        `json:"systemMessage,omitempty"`
}

type hookSpecific struct {
	HookEventName     string `json:"hookEventName,omitempty"`
	AdditionalContext string `json:"additionalContext,omitempty"`
	Message           string `json:"message,omitempty"`
	// PreToolUse-specific fields.
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// HandleHookSessionStart returns recent decisions and open conflicts as
// additionalContext for injection into the IDE session. This replaces the
// old session-start-check.sh that just printed a reminder.
func (h *Handlers) HandleHookSessionStart(w http.ResponseWriter, r *http.Request) {
	var input hookSessionStartInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeHookJSON(w, hookResponse{Continue: true})
		return
	}

	project := inferProjectFromCWD(input.CWD)
	recentContext := h.buildSessionContext(r.Context(), project)

	writeHookJSON(w, hookResponse{
		HookSpecificOutput: &hookSpecific{
			HookEventName:     "SessionStart",
			AdditionalContext: recentContext,
		},
	})
}

// HandleHookPreToolUse handles two cases:
// 1. Edit/Write/MultiEdit gate: blocks until akashi_check has been called recently.
// 2. Pre-commit reminder: suggests calling akashi_check before git commit.
func (h *Handlers) HandleHookPreToolUse(w http.ResponseWriter, r *http.Request) {
	var input hookPreToolUseInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeHookJSON(w, hookResponse{Continue: true})
		return
	}

	switch {
	case isEditTool(input.ToolName):
		if h.hookChecks.IsRecent(input.SessionID) {
			writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})
			return
		}
		writeHookJSON(w, hookResponse{
			HookSpecificOutput: &hookSpecific{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: "Call akashi_check before making changes. This ensures you've checked for prior decisions and conflicts.",
			},
		})

	case isGitCommit(input.ToolInput):
		writeHookJSON(w, hookResponse{
			Continue: true,
			HookSpecificOutput: &hookSpecific{
				HookEventName:     "PreToolUse",
				AdditionalContext: "Consider calling akashi_check before committing to verify no conflicting decisions exist.",
			},
		})

	default:
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})
	}
}

// HandleHookPostToolUse handles:
// 1. akashi_check/akashi_trace completion: records the session check marker.
// 2. Git commit: auto-traces the commit if AutoTrace is enabled.
func (h *Handlers) HandleHookPostToolUse(w http.ResponseWriter, r *http.Request) {
	var input hookPostToolUseInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeHookJSON(w, hookResponse{Continue: true})
		return
	}

	switch {
	case isAkashiTool(input.ToolName):
		h.hookChecks.Record(input.SessionID)
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})

	case isBashTool(input.ToolName) && isGitCommit(input.ToolInput):
		h.handlePostCommit(w, input)

	default:
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})
	}
}

// handlePostCommit auto-traces a git commit and/or suggests manual tracing.
func (h *Handlers) handlePostCommit(w http.ResponseWriter, input hookPostToolUseInput) {
	command := extractCommand(input.ToolInput)
	commitMsg := extractCommitMessage(command)
	if commitMsg == "" {
		commitMsg = "commit (message not parsed)"
	}

	if h.autoTrace {
		go h.autoTraceCommit(input, commitMsg)
		writeHookJSON(w, hookResponse{
			Continue: true,
			HookSpecificOutput: &hookSpecific{
				HookEventName: "PostToolUse",
				Message:       fmt.Sprintf("[akashi] auto-traced commit: %s", truncateHook(commitMsg, 80)),
			},
		})
		return
	}

	writeHookJSON(w, hookResponse{
		Continue: true,
		HookSpecificOutput: &hookSpecific{
			HookEventName: "PostToolUse",
			Message: fmt.Sprintf(
				"[akashi] Call akashi_trace with decision_type=\"implementation\", outcome=%q, confidence=0.8",
				truncateHook(commitMsg, 100),
			),
		},
	})
}

// autoTraceCommit records a decision for a git commit in the background.
// Uses the default org (uuid.Nil) and "admin" agent since hook endpoints
// are unauthenticated.
func (h *Handlers) autoTraceCommit(input hookPostToolUseInput, commitMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	project := inferProjectFromCWD(input.CWD)
	reasoning := "auto-traced from git commit via IDE hook"

	// Use the default org (uuid.Nil) which is created during admin seed.
	orgID := uuid.Nil
	agentID := "admin"

	traceInput := decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "implementation",
			Outcome:      commitMsg,
			Confidence:   0.7,
			Reasoning:    &reasoning,
		},
		AgentContext: map[string]any{
			"source":  "auto-hook",
			"tool":    "ide-hook",
			"project": project,
		},
	}

	if _, err := h.decisionSvc.Trace(ctx, orgID, traceInput); err != nil {
		h.logger.Warn("auto-trace failed", "error", err, "commit", truncateHook(commitMsg, 60))
	}
}

// buildSessionContext creates a compact text summary of recent decisions and conflicts
// for injection into the IDE session context.
func (h *Handlers) buildSessionContext(ctx context.Context, project string) string {
	// Use the default org (uuid.Nil) for unauthenticated hook queries.
	orgID := uuid.Nil

	var parts []string

	// Query recent decisions.
	filters := model.QueryFilters{}
	if project != "" {
		filters.Project = &project
	}
	recent, _, err := h.db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters:  filters,
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    5,
	})
	if err != nil {
		h.logger.Debug("hook session-start: query decisions failed", "error", err)
		recent = nil
	}

	// Query open conflicts.
	openStatus := "open"
	conflictFilter := storage.ConflictFilters{Status: &openStatus}
	conflicts, err := h.db.ListConflicts(ctx, orgID, conflictFilter, 5, 0)
	if err != nil {
		h.logger.Debug("hook session-start: list conflicts failed", "error", err)
		conflicts = nil
	}

	// Build header.
	if project != "" {
		parts = append(parts, fmt.Sprintf("[akashi] Project: %s | %d recent decisions | %d open conflicts",
			project, len(recent), len(conflicts)))
	} else {
		parts = append(parts, fmt.Sprintf("[akashi] %d recent decisions | %d open conflicts",
			len(recent), len(conflicts)))
	}

	// Compact decision summaries.
	if len(recent) > 0 {
		parts = append(parts, "\nRecent decisions:")
		for _, d := range recent {
			age := time.Since(d.CreatedAt)
			ageStr := formatAge(age)
			line := fmt.Sprintf("- [%s] %s (%.0f%% confidence) — %s ago",
				d.DecisionType, truncateHook(d.Outcome, 80), d.Confidence*100, ageStr)
			parts = append(parts, line)
		}
	}

	// Conflict summary.
	if len(conflicts) > 0 {
		parts = append(parts, "\nOpen conflicts:")
		for _, c := range conflicts {
			severity := "unknown"
			if c.Severity != nil {
				severity = *c.Severity
			}
			explanation := ""
			if c.Explanation != nil {
				explanation = ": " + truncateHook(*c.Explanation, 80)
			}
			parts = append(parts, fmt.Sprintf("- [%s] %s vs %s%s", severity, c.AgentA, c.AgentB, explanation))
		}
	}

	parts = append(parts, "\nCall akashi_check before decisions. Call akashi_trace after.")
	return strings.Join(parts, "\n")
}

// --- Helpers ---

var gitCommitRe = regexp.MustCompile(`\bgit\s+commit\b`)

func isEditTool(name string) bool {
	return name == "Edit" || name == "Write" || name == "MultiEdit"
}

func isBashTool(name string) bool {
	return name == "Bash"
}

func isAkashiTool(name string) bool {
	return strings.HasPrefix(name, "mcp__akashi__")
}

func isGitCommit(toolInput map[string]any) bool {
	cmd := extractCommand(toolInput)
	return gitCommitRe.MatchString(cmd)
}

func extractCommand(toolInput map[string]any) string {
	if cmd, ok := toolInput["command"].(string); ok {
		return cmd
	}
	return ""
}

var commitMsgRe = regexp.MustCompile(`git\s+commit\s+(?:.*\s)?-m\s+["']([^"']+)["']`)

func extractCommitMessage(command string) string {
	matches := commitMsgRe.FindStringSubmatch(command)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// inferProjectFromCWD extracts a project name from a directory path,
// preferring the git remote name over the directory basename.
func inferProjectFromCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	if name := gitRepoNameFromPath(cwd); name != "" {
		return name
	}
	return filepath.Base(cwd)
}

// gitRepoNameFromPath runs git to get the origin remote name for a path.
func gitRepoNameFromPath(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	remote := strings.TrimSpace(string(out))
	if remote == "" {
		return ""
	}
	remote = strings.TrimSuffix(remote, ".git")
	return filepath.Base(remote)
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncateHook(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func writeHookJSON(w http.ResponseWriter, resp hookResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("failed to encode hook response", "error", err)
	}
}
