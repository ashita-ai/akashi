package server

import (
	"net/http/httptest"
	"testing"
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
