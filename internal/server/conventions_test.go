package server

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodersExemptFromCheck lists handler files allowed to construct a JSON
// decoder directly. Each entry must have a reason. There are none today, and an
// entry added without a reason should not survive review.
//
// middleware.go is not listed because it is not scanned: the check covers
// handlers*.go only, and middleware.go is where the two legal helpers are
// defined.
var decodersExemptFromCheck = map[string]string{}

// rawDecoderPattern matches a JSON decoder constructed directly over a request
// body — json.NewDecoder(r.Body), json.NewDecoder(req.Body), and so on.
var rawDecoderPattern = regexp.MustCompile(`json\.NewDecoder\(\s*\w+\.Body\s*\)`)

// helperCallPattern matches a call to either sanctioned decode helper.
var helperCallPattern = regexp.MustCompile(`\bdecodeJSON(?:Lenient)?\(`)

// TestHandlersUseDecodeJSONHelpers verifies that no HTTP handler decodes a
// request body with the standard library decoder directly.
//
// This is a security property, not a style preference. decodeJSON and
// decodeJSONLenient (middleware.go) both wrap the body in
// http.MaxBytesReader(w, r.Body, maxBytes) before decoding. A bare
// json.NewDecoder(r.Body) has no such bound, so a single request can stream an
// arbitrary amount of data into the process — a denial-of-service regression
// that no other check in this repo can see. .golangci.yml runs govet,
// staticcheck, unused, ineffassign, errcheck, gosec and gocritic; none of them
// model this rule. Before this test, only human review enforced it, and human
// review is exactly the round that loses a first-time contributor.
//
// BOTH helpers are legal, deliberately. decodeJSON rejects unknown fields;
// decodeJSONLenient does not, because it handles payloads from external senders
// (e.g. Claude Code hooks) that may add fields at any time. A flat "always use
// decodeJSON" rule would push a contributor to tighten the hooks endpoints and
// silently break ingest from any sender that adds a field.
//
// Source-level analysis, following the same approach and reasoning as
// TestOpenAPISpecMatchesRoutes in openapi_test.go: it inspects the text of the
// handler files rather than runtime behaviour, so a handler behind a feature
// flag is still covered.
func TestHandlersUseDecodeJSONHelpers(t *testing.T) {
	dir := sourceRelative(".")

	matches, err := filepath.Glob(filepath.Join(dir, "handlers*.go"))
	require.NoError(t, err, "globbing handler files")
	require.NotEmpty(t, matches, "found no handlers*.go files — the glob is wrong, not the code")

	var violations []string
	helperCallSites := 0
	scanned := 0

	for _, path := range matches {
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		if _, exempt := decodersExemptFromCheck[base]; exempt {
			continue
		}
		scanned++

		data, readErr := os.ReadFile(filepath.Clean(path))
		require.NoError(t, readErr, "reading %s", base)

		for i, line := range strings.Split(string(data), "\n") {
			if rawDecoderPattern.MatchString(line) {
				// 1-indexed to match what an editor shows.
				violations = append(violations,
					fmt.Sprintf("%s:%d  %s", base, i+1, strings.TrimSpace(line)))
			}
			helperCallSites += len(helperCallPattern.FindAllString(line, -1))
		}
	}

	require.NotZero(t, scanned, "no handler files scanned")

	// Defensive, in the same spirit as openapi_test.go asserting it parsed a
	// non-empty route set: if the helpers are renamed and this pattern stops
	// matching, the check above would pass vacuously on a package that had
	// stopped using them entirely. Fail loudly instead.
	require.NotZero(t, helperCallSites,
		"found no decodeJSON/decodeJSONLenient call sites in handlers*.go — the helpers were "+
			"probably renamed, and this guard is now checking nothing. Update helperCallPattern.")

	sort.Strings(violations)
	if len(violations) > 0 {
		assert.Failf(t, "handler decodes a request body without a size bound",
			"These sites construct json.NewDecoder over a request body directly:\n  %s\n\n"+
				"Use one of the helpers in middleware.go instead:\n"+
				"  decodeJSON(w, r, &req, h.maxRequestBodyBytes)         // strict; rejects unknown fields\n"+
				"  decodeJSONLenient(w, r, &req, h.maxRequestBodyBytes)  // for external senders that may add fields\n\n"+
				"Both apply http.MaxBytesReader first. Without it the request body is unbounded, "+
				"which is a denial-of-service regression rather than a style issue.\n"+
				"If a file genuinely must be excluded, add it to decodersExemptFromCheck with a reason.",
			strings.Join(violations, "\n  "))
	}

	t.Logf("scanned %d handler files, %d sanctioned decode call sites, %d violations",
		scanned, helperCallSites, len(violations))
}
