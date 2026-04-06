package server

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// routeFromSource represents a METHOD /path pair extracted from server.go.
type routeFromSource struct {
	Method string
	Path   string
}

func (r routeFromSource) String() string { return r.Method + " " + r.Path }

// routesExcludedFromSpec lists routes that are intentionally absent from the
// OpenAPI spec. Each entry must have a comment explaining why.
var routesExcludedFromSpec = map[string]string{
	// The catch-all SPA handler serves the embedded React UI; it's not an API endpoint.
	"HANDLE /": "SPA catch-all, not an API endpoint",
	// The MCP handler is registered without an explicit method (mux.Handle("/mcp", ...))
	// because the MCP library's StreamableHTTP handler manages GET/POST/DELETE internally.
	// The spec documents the individual methods; see conditionalRoutes for the reverse check.
	"HANDLE /mcp": "MCP StreamableHTTP handler, individual methods documented in spec",
}

// TestOpenAPISpecMatchesRoutes verifies that every HTTP route registered in
// server.go has a corresponding entry in api/openapi.yaml. This prevents
// spec drift — SDK code generation and API documentation depend on the spec
// being complete.
func TestOpenAPISpecMatchesRoutes(t *testing.T) {
	serverGoPath := sourceRelative("server.go")
	openapiPath := sourceRelative("../../api/openapi.yaml")

	registeredRoutes := parseRoutesFromServerGo(t, serverGoPath)
	specRoutes := parseRoutesFromOpenAPI(t, openapiPath)

	// Build a set of spec routes for O(1) lookup.
	specSet := make(map[string]struct{}, len(specRoutes))
	for _, r := range specRoutes {
		specSet[r.String()] = struct{}{}
	}

	// Build a set of registered routes for reverse check.
	registeredSet := make(map[string]struct{}, len(registeredRoutes))
	for _, r := range registeredRoutes {
		registeredSet[r.String()] = struct{}{}
	}

	// Check: every registered route should be in the spec (or excluded).
	var missingFromSpec []string
	for _, r := range registeredRoutes {
		key := r.String()
		if _, excluded := routesExcludedFromSpec[key]; excluded {
			continue
		}
		if _, found := specSet[key]; !found {
			missingFromSpec = append(missingFromSpec, key)
		}
	}
	sort.Strings(missingFromSpec)

	// Check: every spec route should have a registered handler.
	// Routes behind feature flags (signup, hooks, MCP) are conditionally
	// registered but should still be in the spec, so we allow them here.
	conditionalRoutes := map[string]string{
		"POST /auth/signup":         "gated by SignupEnabled config",
		"POST /hooks/session-start": "gated by HooksEnabled config",
		"POST /hooks/pre-tool-use":  "gated by HooksEnabled config",
		"POST /hooks/post-tool-use": "gated by HooksEnabled config",
		"GET /mcp":                  "gated by MCPServer config",
		// The OpenAPI spec may document additional MCP methods handled by the
		// MCP library's StreamableHTTP handler. The server registers a single
		// mux.Handle("/mcp", ...) which matches all methods on that path.
		"POST /mcp":   "handled by MCP StreamableHTTP handler (single registration)",
		"DELETE /mcp": "handled by MCP StreamableHTTP handler (single registration)",
	}

	var missingFromServer []string
	for _, r := range specRoutes {
		key := r.String()
		if _, conditional := conditionalRoutes[key]; conditional {
			continue
		}
		if _, found := registeredSet[key]; !found {
			missingFromServer = append(missingFromServer, key)
		}
	}
	sort.Strings(missingFromServer)

	if len(missingFromSpec) > 0 {
		assert.Failf(t, "routes registered in server.go but missing from openapi.yaml",
			"Add these routes to api/openapi.yaml (or add to routesExcludedFromSpec with a reason):\n  %s",
			strings.Join(missingFromSpec, "\n  "))
	}
	if len(missingFromServer) > 0 {
		assert.Failf(t, "routes in openapi.yaml but not registered in server.go",
			"These spec paths have no handler registered:\n  %s",
			strings.Join(missingFromServer, "\n  "))
	}

	t.Logf("validated %d registered routes against %d spec routes", len(registeredRoutes), len(specRoutes))
}

// parseRoutesFromServerGo reads server.go and extracts all mux.Handle / mux.HandleFunc
// registrations using regex. This is intentionally source-level analysis rather than
// runtime mux inspection — it catches routes behind feature flags that might not be
// registered in a default-config test server.
func parseRoutesFromServerGo(t *testing.T, path string) []routeFromSource {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err, "reading server.go")

	// Match patterns like:
	//   mux.Handle("POST /v1/trace", ...)
	//   mux.HandleFunc("GET /health", ...)
	//   mux.Handle("/mcp", ...)       — no explicit method
	//   mux.Handle("/", ...)           — SPA catch-all
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ )?(/[^"]*)"`)
	matches := re.FindAllSubmatch(data, -1)

	var routes []routeFromSource
	seen := make(map[string]struct{})
	for _, m := range matches {
		method := strings.TrimSpace(string(m[1]))
		path := string(m[2])

		if method == "" {
			// No explicit method — e.g., mux.Handle("/mcp", ...) or mux.Handle("/", ...).
			// For "/", this is the SPA catch-all.
			// For "/mcp", the MCP library handles GET/POST/DELETE internally.
			method = "HANDLE"
		}

		key := method + " " + path
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		routes = append(routes, routeFromSource{Method: method, Path: path})
	}

	require.NotEmpty(t, routes, "failed to parse any routes from server.go — regex may need updating")
	return routes
}

// openAPISpec is the minimal structure we need from the YAML.
type openAPISpec struct {
	Paths map[string]map[string]interface{} `yaml:"paths"`
}

// parseRoutesFromOpenAPI reads the OpenAPI spec and returns all METHOD /path pairs.
func parseRoutesFromOpenAPI(t *testing.T, path string) []routeFromSource {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err, "reading openapi.yaml")

	var spec openAPISpec
	require.NoError(t, yaml.Unmarshal(data, &spec), "parsing openapi.yaml")
	require.NotEmpty(t, spec.Paths, "openapi.yaml has no paths defined")

	httpMethods := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true,
		"delete": true, "options": true, "head": true, "trace": true,
	}

	var routes []routeFromSource
	for p, ops := range spec.Paths {
		for key := range ops {
			if httpMethods[key] {
				routes = append(routes, routeFromSource{
					Method: strings.ToUpper(key),
					Path:   p,
				})
			}
		}
	}

	require.NotEmpty(t, routes, "failed to parse any routes from openapi.yaml")
	return routes
}

// sourceRelative resolves a path relative to this test file's directory.
func sourceRelative(rel string) string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), rel)
}

// TestOpenAPISpecCoversAllMethods checks that when a path exists in the spec,
// all methods registered for that path are documented (not just some).
func TestOpenAPISpecCoversAllMethods(t *testing.T) {
	serverGoPath := sourceRelative("server.go")
	openapiPath := sourceRelative("../../api/openapi.yaml")

	registeredRoutes := parseRoutesFromServerGo(t, serverGoPath)
	specRoutes := parseRoutesFromOpenAPI(t, openapiPath)

	// Group spec routes by path.
	specMethodsByPath := make(map[string]map[string]struct{})
	for _, r := range specRoutes {
		if specMethodsByPath[r.Path] == nil {
			specMethodsByPath[r.Path] = make(map[string]struct{})
		}
		specMethodsByPath[r.Path][r.Method] = struct{}{}
	}

	// For each registered route, if the path exists in the spec but the
	// specific method is missing, that's a partial coverage gap.
	var partialGaps []string
	for _, r := range registeredRoutes {
		if _, excluded := routesExcludedFromSpec[r.String()]; excluded {
			continue
		}
		if r.Method == "HANDLE" {
			continue // catch-all or library-managed
		}
		methods, pathInSpec := specMethodsByPath[r.Path]
		if !pathInSpec {
			continue // already caught by TestOpenAPISpecMatchesRoutes
		}
		if _, hasMethod := methods[r.Method]; !hasMethod {
			partialGaps = append(partialGaps, fmt.Sprintf("%s %s (path exists but %s not documented)", r.Method, r.Path, r.Method))
		}
	}
	sort.Strings(partialGaps)

	if len(partialGaps) > 0 {
		assert.Failf(t, "partial OpenAPI coverage — path exists but method missing",
			"These methods are registered but not in the spec for their path:\n  %s",
			strings.Join(partialGaps, "\n  "))
	}
}
