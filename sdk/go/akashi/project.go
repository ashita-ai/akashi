package akashi

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	inferredProject     string
	inferredProjectOnce sync.Once
)

// inferProjectFromGit resolves the canonical project name by running
// git remote get-url origin in the current working directory and extracting
// the basename (minus .git). The result is cached for the process lifetime
// since the project name won't change within a single process.
//
// Returns "" if git is not available, the directory is not a git repo,
// or no origin remote is configured. Failures are silent — a trace
// without a project is better than a failed trace.
func inferProjectFromGit() string {
	inferredProjectOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
		if err != nil {
			return
		}
		remote := strings.TrimSpace(string(out))
		if remote == "" {
			return
		}
		remote = strings.TrimSuffix(remote, ".git")
		inferredProject = filepath.Base(remote)
	})
	return inferredProject
}
