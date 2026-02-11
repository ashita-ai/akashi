package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves static files from the embedded UI filesystem and falls back
// to index.html for client-side routing. API routes are expected to be registered
// on the mux before the SPA catch-all so they take priority.
type spaHandler struct {
	fs     http.FileSystem
	static http.Handler
}

// newSPAHandler creates an http.Handler that serves the given filesystem as an SPA.
// Hashed assets (those containing a dot in the filename beyond the extension, e.g.
// assets/index-abc123.js) receive immutable cache headers. index.html is served
// with no-cache to ensure clients always fetch the latest version.
func newSPAHandler(fsys fs.FS) http.Handler {
	httpFS := http.FS(fsys)
	return &spaHandler{
		fs:     httpFS,
		static: http.FileServer(httpFS),
	}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Clean the path to prevent directory traversal.
	urlPath := path.Clean(r.URL.Path)
	if urlPath == "." {
		urlPath = "/"
	}

	// API paths that reach the SPA handler were not matched by any route.
	// Return a proper JSON 404 instead of serving index.html.
	if isAPIPath(urlPath) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"endpoint not found"}}`))
		return
	}

	// Try to open the file. If it exists, serve it with appropriate cache headers.
	if urlPath != "/" {
		f, err := h.fs.Open(urlPath)
		if err == nil {
			_ = f.Close()
			setCacheHeaders(w, urlPath)
			h.static.ServeHTTP(w, r)
			return
		}
	}

	// File not found — serve index.html for client-side routing.
	r.URL.Path = "/"
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.static.ServeHTTP(w, r)
}

// isAPIPath returns true if the path belongs to a known API prefix.
// Requests to these paths that reach the SPA handler are genuine 404s.
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/v1/") ||
		strings.HasPrefix(p, "/auth/") ||
		p == "/mcp"
}

// setCacheHeaders sets cache-control headers based on the file path.
// Vite produces hashed filenames in the assets/ directory, so those
// can be cached aggressively. Everything else gets standard caching.
func setCacheHeaders(w http.ResponseWriter, urlPath string) {
	if strings.HasPrefix(urlPath, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
}
