package mcp

import (
	"context"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// validRepoName constrains what parseRepoNameFromURL will accept as a repo
// name. The result is written straight into decisions.project and the
// project_links alias table, so anything that isn't clearly a repo-shaped
// identifier must be rejected at the boundary. GitHub, GitLab, and Bitbucket
// all reject names outside this shape, so no real remote URL is excluded.
var validRepoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// rootsRequestTimeout bounds the synchronous round-trip to the client.
// If the client doesn't respond in time, we skip roots gracefully.
const rootsRequestTimeout = 3 * time.Second

// rootsCache caches MCP roots per session ID so we don't re-request
// on every tool call within the same session. Roots don't change
// mid-session, so one request per session is sufficient.
//
// Failure handling: the first roots failure is NOT cached, allowing one
// retry on the next tool call. If the retry also fails, the empty result
// is cached permanently for the session. This prevents a single transient
// timeout from poisoning all subsequent tool calls in the session.
type rootsCache struct {
	mu      sync.RWMutex
	cache   map[string][]mcplib.Root // sessionID -> roots (only set on success or second failure)
	retried map[string]bool          // sessions that already had one failed attempt
	// NOTE: retried grows by one bool per failed session and is never evicted.
	// This is acceptable because sessions are bounded by connection lifetime
	// and each entry is ~40 bytes. If the server starts handling millions of
	// ephemeral sessions, add a TTL or sync.Map with periodic sweep.
}

func newRootsCache() *rootsCache {
	return &rootsCache{
		cache:   make(map[string][]mcplib.Root),
		retried: make(map[string]bool),
	}
}

// Get returns cached roots for a session, or nil if not cached.
func (rc *rootsCache) Get(sessionID string) ([]mcplib.Root, bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	roots, ok := rc.cache[sessionID]
	return roots, ok
}

// Set caches roots for a session.
func (rc *rootsCache) Set(sessionID string, roots []mcplib.Root) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.cache[sessionID] = roots
}

// ShouldRetry reports whether a failed session should retry (first failure)
// and marks it as retried. Returns false on the second failure, meaning the
// caller should cache the empty result permanently.
func (rc *rootsCache) ShouldRetry(sessionID string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.retried[sessionID] {
		return false // already retried once
	}
	rc.retried[sessionID] = true
	return true
}

// requestRoots requests roots from the client via the MCP protocol,
// caching the result per session ID. Returns nil on any error (roots
// are best-effort context, never blocking).
func (s *Server) requestRoots(ctx context.Context) []mcplib.Root {
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil {
		return nil
	}

	sessionID := session.SessionID()
	if sessionID == "" {
		return nil
	}

	// Check cache first.
	if roots, ok := s.rootsCache.Get(sessionID); ok {
		return roots
	}

	// Request roots from the client. This is a synchronous round-trip
	// (server → client → server), so we bound it with a timeout to avoid
	// blocking the trace if the client is slow or doesn't support roots.
	reqCtx, cancel := context.WithTimeout(ctx, rootsRequestTimeout)
	defer cancel()
	result, err := s.mcpServer.RequestRoots(reqCtx, mcplib.ListRootsRequest{})
	if err != nil {
		// Client may not support roots — that's fine.
		s.logger.Debug("MCP roots request failed (non-fatal)", "error", err, "session_id", sessionID)
		if s.rootsCache.ShouldRetry(sessionID) {
			// First failure: don't cache, allow one retry on the next call.
			return nil
		}
		// Second failure: cache permanently to avoid further round-trips.
		s.rootsCache.Set(sessionID, []mcplib.Root{})
		return nil
	}

	s.rootsCache.Set(sessionID, result.Roots)
	return result.Roots
}

// inferProjectFromRoots extracts a likely project name from the first
// file:// root URI. Returns empty string if no usable root is found.
//
// Examples:
//
//	file:///Users/evan/Documents/gh/akashi/akashi → "akashi"
//	file:///home/user/my-project                  → "my-project"
func inferProjectFromRoots(roots []mcplib.Root) string {
	for _, root := range roots {
		if !strings.HasPrefix(root.URI, "file://") {
			continue
		}
		parsed, err := url.Parse(root.URI)
		if err != nil {
			continue
		}
		path := filepath.Clean(parsed.Path)
		if path == "" || path == "/" || path == "." {
			continue
		}
		return filepath.Base(path)
	}
	return ""
}

// gitRepoName extracts the repository name from the git origin remote URL
// at the given path. Returns "" if the path is not a git repo, has no remote,
// git is not installed, or the call times out.
//
// Uses exec (not shell) so the path is passed as a literal argument — no injection risk.
func gitRepoName(path string) string {
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
	// Normalize: strip .git suffix, then take the last path component.
	// Works for SSH (git@github.com:org/repo.git) and HTTPS (https://github.com/org/repo).
	remote = strings.TrimSuffix(remote, ".git")
	return filepath.Base(remote)
}

// inferProjectFromRootsWithGit extracts a project name from the first file://
// root URI by inspecting its git origin remote. Returns empty string when git
// detection fails (no remote, not a git repo, git not available, or the server
// cannot access the client's filesystem).
//
// Deliberately does NOT fall back to the directory basename — when the server
// runs remotely, the root path is a client-side path (e.g. a Conductor workspace
// directory) whose basename is a workspace name, not a repository name.
func inferProjectFromRootsWithGit(roots []mcplib.Root) string {
	for _, root := range roots {
		if !strings.HasPrefix(root.URI, "file://") {
			continue
		}
		parsed, err := url.Parse(root.URI)
		if err != nil {
			continue
		}
		path := filepath.Clean(parsed.Path)
		if path == "" || path == "/" || path == "." {
			continue
		}
		if name := gitRepoName(path); name != "" {
			return name
		}
	}
	return ""
}

// gitBranchFromRoots extracts the current git branch from the first file://
// root URI by running `git rev-parse --abbrev-ref HEAD`. Returns empty string
// when git detection fails (not a git repo, detached HEAD, git not available,
// or the server cannot access the client's filesystem).
//
// Same access pattern as inferProjectFromRootsWithGit — the server must have
// filesystem access to the root path, which is true for local MCP sessions
// but not for remote ones.
func gitBranchFromRoots(roots []mcplib.Root) string {
	for _, root := range roots {
		if !strings.HasPrefix(root.URI, "file://") {
			continue
		}
		parsed, err := url.Parse(root.URI)
		if err != nil {
			continue
		}
		path := filepath.Clean(parsed.Path)
		if path == "" || path == "/" || path == "." {
			continue
		}
		if branch := gitBranch(path); branch != "" {
			return branch
		}
	}
	return ""
}

// gitBranch returns the current branch name at the given path by running
// `git rev-parse --abbrev-ref HEAD`. Returns "" for detached HEAD, errors,
// or timeout.
func gitBranch(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	// "HEAD" is returned for detached HEAD state — not useful as branch context.
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}

// parseRepoNameFromURL extracts the canonical repo name from a git remote URL.
// Accepts scheme-bearing URLs (https://host/org/repo(.git)?, ssh://…, file://…)
// and SSH shorthand (user@host:org/repo(.git)?). Query strings and fragments
// are stripped. Bare filesystem paths and anything whose final segment isn't a
// repo-shaped identifier return "".
//
// The returned name is flat — git@github.com:ArdentAILabs/mono.git → "mono",
// matching the canonical scheme used elsewhere. Callers that need the org
// prefix must not rely on this function.
func parseRepoNameFromURL(repoURL string) string {
	trimmed := strings.TrimSpace(repoURL)
	if trimmed == "" {
		return ""
	}
	var path string
	switch {
	case strings.Contains(trimmed, "://"):
		// Scheme-bearing URL. net/url gives us the path without query/fragment
		// and with any port on the host already removed.
		u, err := url.Parse(trimmed)
		if err != nil {
			return ""
		}
		path = u.Path
	case !strings.HasPrefix(trimmed, "/"):
		// SSH shorthand: user@host:org/repo(.git)?. The first colon separates
		// the host from the path. Reject empty-path and empty-host forms.
		i := strings.Index(trimmed, ":")
		if i <= 0 || i >= len(trimmed)-1 {
			return ""
		}
		path = trimmed[i+1:]
	default:
		// Bare filesystem path (e.g. "/etc/passwd"). Not a git remote.
		return ""
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	base := filepath.Base(path)
	if !validRepoName.MatchString(base) {
		return ""
	}
	return base
}

// rootURIs extracts the URI strings from a slice of roots.
func rootURIs(roots []mcplib.Root) []string {
	if len(roots) == 0 {
		return nil
	}
	uris := make([]string, len(roots))
	for i, r := range roots {
		uris[i] = r.URI
	}
	return uris
}
