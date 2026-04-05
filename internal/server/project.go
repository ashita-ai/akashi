package server

import (
	"log/slog"
	"net/url"
	"path"
	"strings"
)

// repoNameFromURL extracts a repository name from a git remote URL.
// Handles HTTPS URLs (https://github.com/org/repo.git), SSH URLs
// (git@github.com:org/repo.git), and plain paths. Returns "" if the
// URL is empty or unparseable.
func repoNameFromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}

	// SSH-style URLs: git@github.com:org/repo.git
	if idx := strings.Index(rawURL, ":"); idx > 0 && !strings.Contains(rawURL[:idx], "/") && !strings.Contains(rawURL, "://") {
		after := rawURL[idx+1:]
		name := path.Base(after)
		return strings.TrimSuffix(name, ".git")
	}

	// Standard URLs (https://, ssh://, file://, etc).
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	name := path.Base(parsed.Path)
	if name == "" || name == "." || name == "/" {
		return ""
	}
	return strings.TrimSuffix(name, ".git")
}

// normalizeTraceProject examines the client-supplied context for a trace
// request and resolves the canonical project name. It modifies clientCtx
// in place when normalization finds a better name, and returns an error
// string when the project name is rejected.
//
// Resolution order:
//  1. serverProject — the server-inferred project (from MCP roots + git remote
//     or from the hook's inferProjectFromCWD). Preferred because it comes from
//     a verified source (the server ran git, not the client's self-report).
//  2. repo_url in clientCtx — parsed to extract the repo name.
//  3. Alias lookup via resolveAlias — checks project_links for known mappings.
//  4. Validation against known projects — rejects unknown project names to
//     prevent workspace directory names from leaking into the project field.
//
// When the project name changes, the original value is preserved under the
// "project_submitted" key for the audit trail.
func normalizeTraceProject(
	clientCtx map[string]any,
	serverProject string,
	resolveAlias func(project string) string,
	projectKnown func(project string) bool,
	logger *slog.Logger,
) string {
	clientProject, _ := clientCtx["project"].(string)

	// Step 1: Server-inferred project takes precedence (derived from git remote).
	if serverProject != "" && serverProject != clientProject {
		if clientProject != "" {
			logger.Info("project normalized from server inference",
				"original", clientProject,
				"canonical", serverProject,
			)
			clientCtx["project_submitted"] = clientProject
		}
		clientCtx["project"] = serverProject
		return ""
	}

	// If the server already confirmed the client value, or the client sent nothing, done.
	if clientProject == "" {
		return ""
	}

	// Step 2: Parse repo_url from client context if available.
	if repoURL, ok := clientCtx["repo_url"].(string); ok && repoURL != "" {
		if canonical := repoNameFromURL(repoURL); canonical != "" && canonical != clientProject {
			logger.Info("project normalized from repo_url",
				"original", clientProject,
				"canonical", canonical,
				"repo_url", repoURL,
			)
			clientCtx["project_submitted"] = clientProject
			clientCtx["project"] = canonical
			return ""
		}
	}

	// Step 3: Check project_links for alias mappings.
	if resolveAlias != nil {
		if canonical := resolveAlias(clientProject); canonical != "" {
			logger.Info("project normalized from alias",
				"original", clientProject,
				"canonical", canonical,
			)
			clientCtx["project_submitted"] = clientProject
			clientCtx["project"] = canonical
			return ""
		}
	}

	// Step 4: Validate against known projects. If projectKnown is nil
	// (e.g. test callers that don't wire up the DB), accept the value.
	if projectKnown != nil && !projectKnown(clientProject) {
		logger.Warn("rejected unknown project name",
			"project", clientProject,
		)
		delete(clientCtx, "project")
		return "unknown project " + clientProject + ": provide a valid repo_url or ask an admin to create a project alias"
	}

	return ""
}
