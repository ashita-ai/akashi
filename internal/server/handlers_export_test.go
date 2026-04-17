package server

import "testing"

// TestExportPageSizeOrDefault documents the fallback semantics for Handlers
// constructed with an unset ExportPageSize. Config.Validate enforces the
// documented bounds (1–10000) at load time, but programmatic construction in
// tests or embedders may leave the field zero. A zero page size would cause
// ExportDecisionsCursor to loop without progressing, so we substitute the
// documented default (100) defensively.
func TestExportPageSizeOrDefault(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to default", 0, 100},
		{"negative falls back to default", -5, 100},
		{"one accepted", 1, 1},
		{"explicit value preserved", 250, 250},
		{"large value preserved", 10000, 10000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exportPageSizeOrDefault(tc.in); got != tc.want {
				t.Fatalf("exportPageSizeOrDefault(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewHandlers_ExportPageSizePropagates ensures ServerConfig.ExportPageSize
// reaches Handlers.exportPageSize without mutation (and with the zero-value
// fallback). This is the wiring contract relied on by HandleExportDecisions.
func TestNewHandlers_ExportPageSizePropagates(t *testing.T) {
	t.Run("explicit value propagates", func(t *testing.T) {
		h := NewHandlers(HandlersDeps{ExportPageSize: 500})
		if h.exportPageSize != 500 {
			t.Fatalf("expected exportPageSize 500, got %d", h.exportPageSize)
		}
	})
	t.Run("zero falls back to default", func(t *testing.T) {
		h := NewHandlers(HandlersDeps{})
		if h.exportPageSize != 100 {
			t.Fatalf("expected fallback exportPageSize 100, got %d", h.exportPageSize)
		}
	})
}
