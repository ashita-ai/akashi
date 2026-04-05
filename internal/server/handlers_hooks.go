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

// hookMaxBodyBytes is the maximum request body size for hook endpoints.
// Hook payloads are small JSON objects (tool name, input, session ID), so
// 256 KiB is generous. This prevents unbounded reads when AKASHI_HOOKS_API_KEY
// is configured and the endpoint is exposed beyond localhost.
const hookMaxBodyBytes int64 = 256 << 10

// hookCheckStore tracks when each agent last called akashi_check.
//
// Keyed by agent_id so that one agent's check does not unlock edits for a
// different agent running on the same machine. When the caller does not
// supply an agent_id (e.g. legacy hook scripts), IsAnyRecent provides a
// backwards-compatible fallback that behaves like the old global timestamp.
type hookCheckStore struct {
	mu            sync.RWMutex
	checks        map[string]time.Time // agent_id → last check time
	emptyProjects map[string]time.Time // project → marked-at time (0 decisions + 0 conflicts)
}

const hookCheckTTL = 10 * time.Minute

func newHookCheckStore() *hookCheckStore {
	return &hookCheckStore{
		checks:        make(map[string]time.Time),
		emptyProjects: make(map[string]time.Time),
	}
}

// Record stores a check timestamp for the given agent.
// Empty agentID is ignored — recording "" would let any legacy caller
// (which also has agentID="") pass IsAnyRecent, defeating per-agent isolation.
func (s *hookCheckStore) Record(agentID string) {
	if agentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[agentID] = time.Now()
}

// IsRecent returns true if the given agent called akashi_check within the TTL.
func (s *hookCheckStore) IsRecent(agentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.checks[agentID]
	return ok && time.Since(t) < hookCheckTTL
}

// IsAnyRecent returns true if any agent called akashi_check within the TTL.
// Used as a fallback when the hook request does not include an agent_id.
func (s *hookCheckStore) IsAnyRecent() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.checks {
		if time.Since(t) < hookCheckTTL {
			return true
		}
	}
	return false
}

// Cleanup evicts expired entries from both maps.
func (s *hookCheckStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.checks {
		if time.Since(t) >= hookCheckTTL {
			delete(s.checks, id)
		}
	}
	for project, t := range s.emptyProjects {
		if time.Since(t) >= hookCheckTTL {
			delete(s.emptyProjects, project)
		}
	}
}

// MarkProjectEmpty records that a project has no decisions or conflicts.
// Called during SessionStart when the project's audit trail is empty.
func (s *hookCheckStore) MarkProjectEmpty(project string) {
	if project == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emptyProjects[project] = time.Now()
}

// IsProjectEmpty returns true if the project was marked as having no decisions
// or conflicts within the TTL window. Empty projects skip the edit gate since
// there is nothing to check against.
func (s *hookCheckStore) IsProjectEmpty(project string) bool {
	if project == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.emptyProjects[project]
	return ok && time.Since(t) < hookCheckTTL
}

// ClearProjectEmpty removes the empty marker for a project. Called when a
// decision is traced, so the edit gate re-engages for future checks.
func (s *hookCheckStore) ClearProjectEmpty(project string) {
	if project == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.emptyProjects, project)
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
	AgentID       string         `json:"agent_id"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	HookEventName string         `json:"hook_event_name"`
	CWD           string         `json:"cwd"`
}

// hookPostToolUseInput is the JSON body sent on PostToolUse events.
type hookPostToolUseInput struct {
	SessionID     string         `json:"session_id"`
	AgentID       string         `json:"agent_id"`
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
	if err := decodeJSONLenient(w, r, &input, hookMaxBodyBytes); err != nil {
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

// HandleHookPreToolUse gates Edit/Write/MultiEdit until akashi_check has been
// called recently. Non-edit tools pass through immediately.
//
// Note: Bash is intentionally excluded from the PreToolUse matcher to avoid
// firing a network round-trip on every shell command. The pre-commit hint
// (previously handled here) was not worth intercepting ~50 Bash calls per
// session to catch ~2 git commits. Commit tracing is handled in PostToolUse.
func (h *Handlers) HandleHookPreToolUse(w http.ResponseWriter, r *http.Request) {
	var input hookPreToolUseInput
	if err := decodeJSONLenient(w, r, &input, hookMaxBodyBytes); err != nil {
		writeHookJSON(w, hookResponse{Continue: true})
		return
	}

	if !isEditTool(input.ToolName) {
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})
		return
	}

	// Fast path: in-memory check before shelling out to git.
	allowed := false
	if input.AgentID != "" {
		allowed = h.hookChecks.IsRecent(input.AgentID)
	} else {
		// No agent_id in request — fall back to checking any agent.
		// This preserves backwards compatibility with older hook scripts.
		allowed = h.hookChecks.IsAnyRecent()
	}
	if allowed {
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})
		return
	}

	// Short-circuit: if the project has no decision history, don't gate edits.
	// The empty marker is set during SessionStart and cleared on first trace.
	// This check comes after IsRecent because inferProjectFromCWD spawns a
	// git subprocess, whereas IsRecent is a pure map lookup.
	if project := inferProjectFromCWD(input.CWD); h.hookChecks.IsProjectEmpty(project) {
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
}

// HandleHookPostToolUse handles:
// 1. akashi_check/akashi_trace completion: records the session check marker.
// 2. Git commit: auto-traces the commit if AutoTrace is enabled.
func (h *Handlers) HandleHookPostToolUse(w http.ResponseWriter, r *http.Request) {
	var input hookPostToolUseInput
	if err := decodeJSONLenient(w, r, &input, hookMaxBodyBytes); err != nil {
		writeHookJSON(w, hookResponse{Continue: true})
		return
	}

	switch {
	case isAkashiTool(input.ToolName):
		h.hookChecks.Record(input.AgentID)
		// If a decision was just traced, the project is no longer empty.
		if strings.Contains(input.ToolName, "trace") {
			if project := inferProjectFromCWD(input.CWD); project != "" {
				h.hookChecks.ClearProjectEmpty(project)
			}
		}
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})

	case isBashTool(input.ToolName) && isGitCommit(input.ToolInput):
		h.handlePostCommit(w, input)

	default:
		writeHookJSON(w, hookResponse{Continue: true, SuppressOutput: true})
	}
}

// handlePostCommit auto-traces a git commit and/or suggests manual tracing.
//
// The commit has already happened by the time PostToolUse fires, so we read
// the subject from `git log -1 --format=%s` instead of parsing the command
// string. This correctly handles HEREDOC commits, --amend, and editor-based
// messages that the old regex-based parser could not.
func (h *Handlers) handlePostCommit(w http.ResponseWriter, input hookPostToolUseInput) {
	commitMsg := gitCommitSubject(input.CWD)
	if commitMsg == "" {
		// Fallback: try parsing from the command string.
		commitMsg = extractCommitMessage(extractCommand(input.ToolInput))
	}
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
				"[akashi] Call akashi_trace with decision_type=\"implementation\", outcome=%q, confidence=0.6",
				truncateHook(commitMsg, 100),
			),
		},
	})
}

// autoTraceCommit records a decision for a git commit in the background.
// Uses the default org (uuid.Nil) and "admin" agent since hook endpoints
// are unauthenticated.
//
// Enrichments over a bare trace:
//   - Evidence: changed file paths from git diff HEAD~1
//   - Task: branch name (common prefixes stripped)
//   - Reasoning: commit message body (lines after the subject), if present
//   - Confidence: 0.5 (mechanical auto-trace, not a judgment call)
func (h *Handlers) autoTraceCommit(input hookPostToolUseInput, commitMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	project := inferProjectFromCWD(input.CWD)
	if project == "" {
		h.logger.Warn("auto-trace skipped: could not detect project from git remote", "cwd", input.CWD)
		return
	}

	// Extract reasoning from the commit body. Fall back to generic label.
	reasoning := "auto-traced from git commit via IDE hook"
	if body := gitCommitBody(input.CWD); body != "" {
		reasoning = body
	}

	// Build evidence from the changed file list.
	var evidence []model.TraceEvidence
	if diff := gitDiffNameOnly(input.CWD); diff != "" {
		sourceURI := "git:diff"
		evidence = append(evidence, model.TraceEvidence{
			SourceType: "tool_output",
			SourceURI:  &sourceURI,
			Content:    diff,
		})
	}

	// Use branch name (stripped of prefix) as the task label.
	agentCtx := map[string]any{
		"source":  "auto-hook",
		"tool":    "ide-hook",
		"project": project,
	}
	if task := gitBranchTask(input.CWD); task != "" {
		agentCtx["task"] = task
	}

	orgID := uuid.Nil
	agentID := "admin"

	traceInput := decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "implementation",
			Outcome:      commitMsg,
			Confidence:   0.5,
			Reasoning:    &reasoning,
			Evidence:     evidence,
		},
		AgentContext: agentCtx,
	}

	if _, err := h.decisionSvc.Trace(ctx, orgID, traceInput); err != nil {
		h.logger.Warn("auto-trace failed", "error", err, "commit", truncateHook(commitMsg, 60))
	} else {
		h.hookChecks.ClearProjectEmpty(project)
	}
}

// gitDiffNameOnly returns the newline-separated list of files changed in
// the most recent commit, or "" on any error.
func gitDiffNameOnly(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "diff", "--name-only", "HEAD~1").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitCommitSubject returns the subject line of the most recent commit,
// or "" on error. This is preferred over regex-parsing the command string
// because it handles HEREDOC commits, --amend, and editor-based messages.
func gitCommitSubject(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "log", "-1", "--format=%s").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitCommitBody returns the body (lines after the subject) of the most
// recent commit, or "" if there is no body or on error.
func gitCommitBody(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "log", "-1", "--format=%b").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitBranchTask returns the current branch name with common prefixes
// (feature/, fix/, evanvolgas/, etc.) stripped, suitable for use as a
// task label. Returns "" on error or detached HEAD.
func gitBranchTask(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "" // detached HEAD
	}
	return stripBranchPrefix(branch)
}

// stripBranchPrefix removes conventional workflow prefixes so the task
// label reflects the intent, not the branching convention. Only known
// prefixes are stripped — unknown prefixes (usernames, "release/", etc.)
// are left intact to preserve meaningful context in the audit trail.
func stripBranchPrefix(branch string) string {
	prefixes := []string{
		"feature/", "fix/", "bugfix/", "hotfix/",
		"chore/", "refactor/", "docs/", "test/",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(branch, p) && len(branch) > len(p) {
			return branch[len(p):]
		}
	}
	return branch
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

	// Query open conflicts — scoped by project when known.
	openStatus := "open"
	conflictFilter := storage.ConflictFilters{Status: &openStatus}
	if project != "" {
		conflictFilter.Project = &project
	}
	conflicts, err := h.db.ListConflicts(ctx, orgID, conflictFilter, 5, 0)
	if err != nil {
		h.logger.Debug("hook session-start: list conflicts failed", "error", err)
		conflicts = nil
	}

	// Track whether this project has any decision history.
	// Empty projects get a relaxed edit gate in HandleHookPreToolUse.
	if project != "" {
		if len(recent) == 0 && len(conflicts) == 0 {
			h.hookChecks.MarkProjectEmpty(project)
		} else {
			h.hookChecks.ClearProjectEmpty(project)
		}
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

	parts = append(parts, "\nCall akashi_check before architecture/design decisions. Call akashi_trace after.")
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

// inferProjectFromCWD extracts a project name from a directory path using the
// git origin remote. Returns empty string when git detection fails. Does NOT
// fall back to directory basename — for orchestration tools like Conductor,
// the basename is a workspace name, not the repository name.
func inferProjectFromCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	return gitRepoNameFromPath(cwd)
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

// truncateHook truncates s to at most maxRunes runes, appending "..." if
// truncated. Operates on runes, not bytes, to avoid splitting multi-byte
// UTF-8 characters (e.g. CJK, emoji) mid-codepoint.
func truncateHook(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func writeHookJSON(w http.ResponseWriter, resp hookResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("failed to encode hook response", "error", err)
	}
}
