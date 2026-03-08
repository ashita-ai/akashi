package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsAPIPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// API paths that should be detected.
		{"/v1/trace", true},
		{"/v1/query", true},
		{"/v1/runs/some-id", true},
		{"/v1/agents", true},
		{"/v1/decisions/recent", true},
		{"/v1/", true},
		{"/auth/token", true},
		{"/auth/refresh", true},
		{"/auth/verify", true},
		{"/mcp", true},

		// Non-API paths that the SPA should handle.
		{"/", false},
		{"/decisions", false},
		{"/agents", false},
		{"/settings", false},
		{"/assets/index-abc123.js", false},
		{"/favicon.ico", false},
		{"/health", false}, // Health is registered on the mux, not an API path for SPA purposes.
		{"/config", false}, // Config is a public endpoint, not an API prefix.
		{"/openapi.yaml", false},
		{"/some/other/path", false},

		// Edge cases.
		{"", false},
		{"/v1", false},     // Must have trailing slash to match /v1/ prefix.
		{"/v2/foo", false}, // Different API version is not recognized.
		{"/authorization", false},
		{"/mcpserver", false}, // /mcp must match exactly, not as a prefix.
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isAPIPath(tt.path)
			if got != tt.want {
				t.Errorf("isAPIPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSetCacheHeaders(t *testing.T) {
	tests := []struct {
		name    string
		urlPath string
		wantCC  string // expected Cache-Control header value
	}{
		{
			name:    "hashed asset gets immutable cache",
			urlPath: "/assets/index-abc123.js",
			wantCC:  "public, max-age=31536000, immutable",
		},
		{
			name:    "hashed CSS asset gets immutable cache",
			urlPath: "/assets/style-def456.css",
			wantCC:  "public, max-age=31536000, immutable",
		},
		{
			name:    "assets directory root gets immutable cache",
			urlPath: "/assets/something",
			wantCC:  "public, max-age=31536000, immutable",
		},
		{
			name:    "non-asset file gets standard cache",
			urlPath: "/favicon.ico",
			wantCC:  "public, max-age=3600",
		},
		{
			name:    "root path gets standard cache",
			urlPath: "/index.html",
			wantCC:  "public, max-age=3600",
		},
		{
			name:    "nested non-asset path gets standard cache",
			urlPath: "/images/logo.png",
			wantCC:  "public, max-age=3600",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			setCacheHeaders(w, tt.urlPath)
			got := w.Header().Get("Cache-Control")
			if got != tt.wantCC {
				t.Errorf("setCacheHeaders(%q): Cache-Control = %q, want %q", tt.urlPath, got, tt.wantCC)
			}
		})
	}
}

// testSPAFS returns a minimal in-memory filesystem for SPA handler tests.
// Contains index.html at the root and a hashed asset in the assets/ directory.
func testSPAFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><html><body>SPA</body></html>")},
		"assets/index-abc123.js":  {Data: []byte("console.log('app')")},
		"assets/style-def456.css": {Data: []byte("body{margin:0}")},
		"favicon.ico":             {Data: []byte("icon-data")},
	}
}

func TestSPAHandler_ServesExistingFile(t *testing.T) {
	handler := newSPAHandler(testSPAFS())

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
		wantCC     string
	}{
		{
			name:       "serves hashed JS asset with immutable cache",
			path:       "/assets/index-abc123.js",
			wantStatus: http.StatusOK,
			wantBody:   "console.log('app')",
			wantCC:     "public, max-age=31536000, immutable",
		},
		{
			name:       "serves hashed CSS asset with immutable cache",
			path:       "/assets/style-def456.css",
			wantStatus: http.StatusOK,
			wantBody:   "body{margin:0}",
			wantCC:     "public, max-age=31536000, immutable",
		},
		{
			name:       "serves favicon with standard cache",
			path:       "/favicon.ico",
			wantStatus: http.StatusOK,
			wantBody:   "icon-data",
			wantCC:     "public, max-age=3600",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.wantBody)
			assert.Equal(t, tt.wantCC, rec.Header().Get("Cache-Control"))
		})
	}
}

func TestSPAHandler_FallsBackToIndex(t *testing.T) {
	handler := newSPAHandler(testSPAFS())

	// Client-side routes that don't correspond to real files should serve index.html.
	tests := []struct {
		name string
		path string
	}{
		{"root path", "/"},
		{"SPA route /decisions", "/decisions"},
		{"SPA route /agents/foo", "/agents/foo"},
		{"SPA route /settings", "/settings"},
		{"nonexistent file", "/does-not-exist.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.Equal(t, "no-cache, no-store, must-revalidate", rec.Header().Get("Cache-Control"))
		})
	}
}

func TestSPAHandler_APIPathReturnsJSON404(t *testing.T) {
	handler := newSPAHandler(testSPAFS())

	// API paths that reach the SPA handler (not matched by any mux route) should
	// return a proper JSON 404 instead of serving index.html.
	tests := []struct {
		name string
		path string
	}{
		{"v1 API path", "/v1/nonexistent"},
		{"auth path", "/auth/nonexistent"},
		{"mcp path", "/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

			body, err := io.ReadAll(rec.Body)
			require.NoError(t, err)

			var errResp map[string]map[string]string
			require.NoError(t, json.Unmarshal(body, &errResp))
			assert.Equal(t, "not_found", errResp["error"]["code"])
			assert.Equal(t, "endpoint not found", errResp["error"]["message"])
		})
	}
}

func TestSPAHandler_PathTraversal(t *testing.T) {
	handler := newSPAHandler(testSPAFS())

	// Directory traversal attempts should be cleaned and fall back to index.html.
	req := httptest.NewRequest("GET", "/../../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// path.Clean("/../../../etc/passwd") => "/etc/passwd" which is not an API path
	// and doesn't exist in the FS, so it falls back to index.html.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestSPAHandler_DotPath(t *testing.T) {
	handler := newSPAHandler(testSPAFS())

	// A request to "." (after cleaning) should be treated as root and serve index.html.
	req := httptest.NewRequest("GET", "/.", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-cache, no-store, must-revalidate", rec.Header().Get("Cache-Control"))
}
