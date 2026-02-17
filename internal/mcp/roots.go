package mcp

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// rootsRequestTimeout bounds the synchronous round-trip to the client.
// If the client doesn't respond in time, we skip roots gracefully.
const rootsRequestTimeout = 3 * time.Second

// rootsCache caches MCP roots per session ID so we don't re-request
// on every tool call within the same session. Roots don't change
// mid-session, so one request per session is sufficient.
type rootsCache struct {
	mu    sync.RWMutex
	cache map[string][]mcplib.Root // sessionID -> roots
}

func newRootsCache() *rootsCache {
	return &rootsCache{
		cache: make(map[string][]mcplib.Root),
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
		// Cache empty slice to avoid re-requesting.
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
