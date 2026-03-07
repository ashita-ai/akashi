package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzDecodeJSON exercises JSON decoding with arbitrary payloads.
// This covers malformed JSON, oversized bodies, type mismatches, and
// unexpected field shapes without requiring a live server.
func FuzzDecodeJSON(f *testing.F) {
	// Seed with valid JSON matching a common request shape.
	f.Add([]byte(`{"decision_type":"architecture","outcome":"microservices","confidence":0.85}`), int64(1048576))
	f.Add([]byte(`{}`), int64(1048576))
	f.Add([]byte(`{"nested":{"a":1}}`), int64(1048576))

	// Malformed JSON.
	f.Add([]byte(`{`), int64(1048576))
	f.Add([]byte(`{"key": `), int64(1048576))
	f.Add([]byte(``), int64(1048576))
	f.Add([]byte(`null`), int64(1048576))
	f.Add([]byte(`[1,2,3]`), int64(1048576))

	// Oversized body (small limit).
	f.Add([]byte(`{"decision_type":"architecture","outcome":"microservices"}`), int64(10))

	// Type mismatches.
	f.Add([]byte(`{"confidence":"not_a_number"}`), int64(1048576))
	f.Add([]byte(`{"confidence":999999999999999999999999}`), int64(1048576))

	// Binary garbage.
	f.Add([]byte{0x00, 0x01, 0xFF, 0xFE}, int64(1048576))

	// Deeply nested.
	f.Add([]byte(`{"a":{"b":{"c":{"d":{"e":"deep"}}}}}`), int64(1048576))

	type traceRequest struct {
		DecisionType string   `json:"decision_type"`
		Outcome      string   `json:"outcome"`
		Confidence   *float64 `json:"confidence"`
		Reasoning    *string  `json:"reasoning"`
	}

	f.Fuzz(func(t *testing.T, body []byte, maxBytes int64) {
		// Clamp maxBytes to reasonable range.
		if maxBytes < 1 {
			maxBytes = 1
		}
		if maxBytes > 16<<20 {
			maxBytes = 16 << 20
		}

		w := httptest.NewRecorder()
		r := &http.Request{
			Body: io.NopCloser(bytes.NewReader(body)),
		}

		var target traceRequest
		// Must not panic. Errors are expected for fuzzed inputs.
		_ = decodeJSON(w, r, &target, maxBytes)
	})
}
