package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
)

// quietLogger returns a quiet logger for use in middleware tests.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- requestIDMiddleware ---

func TestRequestIDMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the request ID from context back in the body so we can inspect it.
		reqID := RequestIDFromContext(r.Context())
		_, _ = w.Write([]byte(reqID))
	})
	handler := requestIDMiddleware(inner)

	t.Run("generates UUID when no header provided", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		handler.ServeHTTP(rec, req)

		headerID := rec.Header().Get("X-Request-ID")
		assert.NotEmpty(t, headerID, "should set X-Request-ID response header")
		_, err := uuid.Parse(headerID)
		assert.NoError(t, err, "generated ID should be a valid UUID")
		assert.Equal(t, headerID, rec.Body.String(), "context ID should match header ID")
	})

	t.Run("accepts valid client-supplied ID", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Request-ID", "my-custom-request-id-123")
		handler.ServeHTTP(rec, req)

		assert.Equal(t, "my-custom-request-id-123", rec.Header().Get("X-Request-ID"))
		assert.Equal(t, "my-custom-request-id-123", rec.Body.String())
	})

	t.Run("rejects empty ID and generates UUID", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Request-ID", "")
		handler.ServeHTTP(rec, req)

		headerID := rec.Header().Get("X-Request-ID")
		_, err := uuid.Parse(headerID)
		assert.NoError(t, err, "should generate a valid UUID for empty ID")
	})

	t.Run("rejects ID with control characters", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Request-ID", "bad\x00id")
		handler.ServeHTTP(rec, req)

		headerID := rec.Header().Get("X-Request-ID")
		assert.NotEqual(t, "bad\x00id", headerID, "should not echo ID with control chars")
		_, err := uuid.Parse(headerID)
		assert.NoError(t, err, "should generate a valid UUID")
	})

	t.Run("rejects ID exceeding 128 characters", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		longID := strings.Repeat("a", 129)
		req.Header.Set("X-Request-ID", longID)
		handler.ServeHTTP(rec, req)

		headerID := rec.Header().Get("X-Request-ID")
		assert.NotEqual(t, longID, headerID)
		_, err := uuid.Parse(headerID)
		assert.NoError(t, err)
	})

	t.Run("accepts ID at exactly 128 characters", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		maxID := strings.Repeat("x", 128)
		req.Header.Set("X-Request-ID", maxID)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, maxID, rec.Header().Get("X-Request-ID"))
	})

	t.Run("rejects ID with non-ASCII characters", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Request-ID", "id-with-\xc0\xff")
		handler.ServeHTTP(rec, req)

		headerID := rec.Header().Get("X-Request-ID")
		_, err := uuid.Parse(headerID)
		assert.NoError(t, err, "should generate a valid UUID for non-ASCII ID")
	})
}

func TestIsValidRequestID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"empty string", "", false},
		{"simple alphanumeric", "abc123", true},
		{"UUID format", "550e8400-e29b-41d4-a716-446655440000", true},
		{"printable ASCII with special chars", "req-id_123.456", true},
		{"space is printable", "req id", true},
		{"tilde is printable", "req~id", true},
		{"at max length", strings.Repeat("a", 128), true},
		{"over max length", strings.Repeat("a", 129), false},
		{"null byte", "abc\x00def", false},
		{"newline", "abc\ndef", false},
		{"tab", "abc\tdef", false},
		{"DEL character (0x7f)", "abc\x7fdef", false},
		{"high byte (0x80)", "abc\x80def", false},
		{"non-ASCII UTF-8", "caf\xc3\xa9", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidRequestID(tt.id))
		})
	}
}

// --- RequestIDFromContext ---

func TestRequestIDFromContext(t *testing.T) {
	t.Run("returns empty string for bare context", func(t *testing.T) {
		assert.Equal(t, "", RequestIDFromContext(context.Background()))
	})

	t.Run("returns stored request ID", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), contextKeyRequestID, "test-id-42")
		assert.Equal(t, "test-id-42", RequestIDFromContext(ctx))
	})

	t.Run("returns empty string for wrong type in context", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), contextKeyRequestID, 42)
		assert.Equal(t, "", RequestIDFromContext(ctx))
	})
}

// --- loggingMiddleware ---

func TestLoggingMiddleware(t *testing.T) {
	logger := quietLogger()

	t.Run("passes through to inner handler", func(t *testing.T) {
		called := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusCreated)
		})
		handler := loggingMiddleware(logger, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/trace", nil)
		handler.ServeHTTP(rec, req)

		assert.True(t, called)
		assert.Equal(t, http.StatusCreated, rec.Code)
	})

	t.Run("captures status code from WriteHeader", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		handler := loggingMiddleware(logger, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/missing", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("defaults to 200 when WriteHeader not called", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})
		handler := loggingMiddleware(logger, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/health", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- statusWriter ---

func TestStatusWriter(t *testing.T) {
	t.Run("WriteHeader records status code", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: rec, statusCode: http.StatusOK}
		sw.WriteHeader(http.StatusBadRequest)
		assert.Equal(t, http.StatusBadRequest, sw.statusCode)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("Unwrap returns underlying ResponseWriter", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: rec, statusCode: http.StatusOK}
		assert.Equal(t, rec, sw.Unwrap())
	})

	t.Run("Flush delegates to underlying Flusher", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: rec, statusCode: http.StatusOK}
		// httptest.ResponseRecorder implements http.Flusher, so this should not panic.
		sw.Flush()
		assert.True(t, rec.Flushed)
	})
}

// --- recoveryMiddleware ---

func TestRecoveryMiddleware(t *testing.T) {
	logger := quietLogger()

	t.Run("recovers from panic and returns 500", func(t *testing.T) {
		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			panic("test panic")
		})
		handler := recoveryMiddleware(logger, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/explode", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)

		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Equal(t, model.ErrCodeInternalError, errResp.Error.Code)
		assert.Equal(t, "internal server error", errResp.Error.Message)
	})

	t.Run("passes through normally when no panic", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		handler := recoveryMiddleware(logger, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/safe", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "ok", rec.Body.String())
	})

	t.Run("recovers from non-string panic value", func(t *testing.T) {
		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			panic(42)
		})
		handler := recoveryMiddleware(logger, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/explode", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// --- corsMiddleware ---

func TestCorsMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("allows listed origin", func(t *testing.T) {
		handler := corsMiddleware([]string{"https://app.example.com"}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Origin", "https://app.example.com")
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	})

	t.Run("rejects unlisted origin", func(t *testing.T) {
		handler := corsMiddleware([]string{"https://app.example.com"}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Origin", "https://evil.com")
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), "unlisted origin should not be reflected")
		assert.Equal(t, "Origin", rec.Header().Get("Vary"), "Vary: Origin should always be set")
	})

	t.Run("wildcard allows any origin", func(t *testing.T) {
		handler := corsMiddleware([]string{"*"}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Origin", "https://anything.example.org")
		handler.ServeHTTP(rec, req)

		assert.Equal(t, "https://anything.example.org", rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("OPTIONS preflight returns 204", func(t *testing.T) {
		handler := corsMiddleware([]string{"https://app.example.com"}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/v1/agents", nil)
		req.Header.Set("Origin", "https://app.example.com")
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "86400", rec.Header().Get("Access-Control-Max-Age"))
	})

	t.Run("OPTIONS without matching origin still returns 204", func(t *testing.T) {
		handler := corsMiddleware([]string{"https://app.example.com"}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/v1/agents", nil)
		req.Header.Set("Origin", "https://evil.com")
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("no origin header passes through without CORS headers", func(t *testing.T) {
		handler := corsMiddleware([]string{"https://app.example.com"}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "Origin", rec.Header().Get("Vary"))
	})

	t.Run("empty allowed origins list rejects all origins", func(t *testing.T) {
		handler := corsMiddleware([]string{}, inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Origin", "https://any.com")
		handler.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

// --- securityHeadersMiddleware ---

func TestSecurityHeadersMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := securityHeadersMiddleware(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
	assert.Contains(t, rec.Header().Get("Strict-Transport-Security"), "max-age=")
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'self'")
	assert.Contains(t, rec.Header().Get("Permissions-Policy"), "camera=()")
}

// --- requireRole ---

func TestRequireRole(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authorized"))
	})

	t.Run("allows request when role meets minimum", func(t *testing.T) {
		handler := requireRole(model.RoleAgent)(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		claims := &auth.Claims{AgentID: "test", Role: model.RoleAdmin, OrgID: uuid.New()}
		ctx := ctxutil.WithClaims(req.Context(), claims)
		handler.ServeHTTP(rec, req.WithContext(ctx))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "authorized", rec.Body.String())
	})

	t.Run("allows request when role equals minimum", func(t *testing.T) {
		handler := requireRole(model.RoleAgent)(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		claims := &auth.Claims{AgentID: "test", Role: model.RoleAgent, OrgID: uuid.New()}
		ctx := ctxutil.WithClaims(req.Context(), claims)
		handler.ServeHTTP(rec, req.WithContext(ctx))

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("rejects request when role below minimum", func(t *testing.T) {
		handler := requireRole(model.RoleAdmin)(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		claims := &auth.Claims{AgentID: "test", Role: model.RoleReader, OrgID: uuid.New()}
		ctx := ctxutil.WithClaims(req.Context(), claims)
		handler.ServeHTTP(rec, req.WithContext(ctx))

		assert.Equal(t, http.StatusForbidden, rec.Code)
		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Equal(t, model.ErrCodeForbidden, errResp.Error.Code)
	})

	t.Run("returns 401 when no claims in context", func(t *testing.T) {
		handler := requireRole(model.RoleReader)(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Equal(t, model.ErrCodeUnauthorized, errResp.Error.Code)
	})

	t.Run("platform_admin passes any role check", func(t *testing.T) {
		handler := requireRole(model.RoleAdmin)(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		claims := &auth.Claims{AgentID: "superadmin", Role: model.RolePlatformAdmin, OrgID: uuid.New()}
		ctx := ctxutil.WithClaims(req.Context(), claims)
		handler.ServeHTTP(rec, req.WithContext(ctx))

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- clientIP ---

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trustProxy bool
		want       string
	}{
		{
			name:       "RemoteAddr with port",
			remoteAddr: "192.168.1.1:12345",
			trustProxy: false,
			want:       "192.168.1.1",
		},
		{
			name:       "RemoteAddr without port",
			remoteAddr: "192.168.1.1",
			trustProxy: false,
			want:       "192.168.1.1",
		},
		{
			name:       "XFF ignored when trustProxy is false",
			remoteAddr: "10.0.0.1:8080",
			xff:        "203.0.113.50",
			trustProxy: false,
			want:       "10.0.0.1",
		},
		{
			name:       "XFF used when trustProxy is true single IP",
			remoteAddr: "10.0.0.1:8080",
			xff:        "203.0.113.50",
			trustProxy: true,
			want:       "203.0.113.50",
		},
		{
			name:       "XFF uses leftmost IP from chain",
			remoteAddr: "10.0.0.1:8080",
			xff:        "203.0.113.50, 70.41.3.18, 150.172.238.178",
			trustProxy: true,
			want:       "203.0.113.50",
		},
		{
			name:       "XFF trims whitespace",
			remoteAddr: "10.0.0.1:8080",
			xff:        "  203.0.113.50  ",
			trustProxy: true,
			want:       "203.0.113.50",
		},
		{
			name:       "empty XFF falls back to RemoteAddr",
			remoteAddr: "10.0.0.1:8080",
			xff:        "",
			trustProxy: true,
			want:       "10.0.0.1",
		},
		{
			name:       "IPv6 RemoteAddr with port",
			remoteAddr: "[::1]:8080",
			trustProxy: false,
			want:       "::1",
		},
		{
			name:       "empty host falls back to full RemoteAddr",
			remoteAddr: ":8080",
			trustProxy: false,
			want:       ":8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			got := clientIP(req, tt.trustProxy)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- routePattern ---

func TestRoutePattern(t *testing.T) {
	t.Run("uses r.Pattern when set", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/decisions/recent", nil)
		req.Pattern = "GET /v1/decisions/recent"
		assert.Equal(t, "GET /v1/decisions/recent", routePattern(req))
	})

	t.Run("falls back to first two path segments when Pattern is empty", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/decisions/some-uuid", nil)
		// Pattern is empty by default.
		assert.Equal(t, "GET /v1/decisions", routePattern(req))
	})

	t.Run("short path returns method + path", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/health", nil)
		assert.Equal(t, "GET /health", routePattern(req))
	})

	t.Run("root path returns method + path", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		assert.Equal(t, "GET /", routePattern(req))
	})
}

// --- writeJSON ---

func TestWriteJSON(t *testing.T) {
	t.Run("writes standard envelope with data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		ctx := context.WithValue(req.Context(), contextKeyRequestID, "req-123")

		writeJSON(rec, req.WithContext(ctx), http.StatusOK, map[string]string{"key": "value"})

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var resp model.APIResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, "req-123", resp.Meta.RequestID)
		assert.False(t, resp.Meta.Timestamp.IsZero())

		// Data is unmarshaled as a generic map.
		data, ok := resp.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "value", data["key"])
	})

	t.Run("writes non-200 status codes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/test", nil)

		writeJSON(rec, req, http.StatusCreated, map[string]int{"id": 1})

		assert.Equal(t, http.StatusCreated, rec.Code)
	})

	t.Run("writes nil data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)

		writeJSON(rec, req, http.StatusOK, nil)

		assert.Equal(t, http.StatusOK, rec.Code)
		var resp model.APIResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Nil(t, resp.Data)
	})
}

// --- writeError ---

func TestWriteError(t *testing.T) {
	t.Run("writes error envelope", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		ctx := context.WithValue(req.Context(), contextKeyRequestID, "err-456")

		writeError(rec, req.WithContext(ctx), http.StatusBadRequest, model.ErrCodeInvalidInput, "field is required")

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
		assert.Equal(t, "field is required", errResp.Error.Message)
		assert.Equal(t, "err-456", errResp.Meta.RequestID)
	})

	t.Run("writes 500 error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)

		writeError(rec, req, http.StatusInternalServerError, model.ErrCodeInternalError, "something broke")

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// --- decodeJSON and handleDecodeError ---

func TestDecodeJSON(t *testing.T) {
	type payload struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	}

	t.Run("decodes valid JSON", func(t *testing.T) {
		body := `{"name":"test","value":3.14}`
		rec := httptest.NewRecorder()
		req := &http.Request{Body: io.NopCloser(strings.NewReader(body))}

		var p payload
		err := decodeJSON(rec, req, &p, 1024)
		require.NoError(t, err)
		assert.Equal(t, "test", p.Name)
		assert.InDelta(t, 3.14, p.Value, 0.001)
	})

	t.Run("returns error for malformed JSON", func(t *testing.T) {
		body := `{bad json`
		rec := httptest.NewRecorder()
		req := &http.Request{Body: io.NopCloser(strings.NewReader(body))}

		var p payload
		err := decodeJSON(rec, req, &p, 1024)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, errBodyTooLarge)
	})

	t.Run("returns errBodyTooLarge for oversized body", func(t *testing.T) {
		body := `{"name":"this is a long name that exceeds the limit"}`
		rec := httptest.NewRecorder()
		req := &http.Request{Body: io.NopCloser(strings.NewReader(body))}

		var p payload
		err := decodeJSON(rec, req, &p, 10) // 10 bytes max
		assert.ErrorIs(t, err, errBodyTooLarge)
	})

	t.Run("returns error for empty body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := &http.Request{Body: io.NopCloser(strings.NewReader(""))}

		var p payload
		err := decodeJSON(rec, req, &p, 1024)
		assert.Error(t, err)
	})
}

func TestHandleDecodeError(t *testing.T) {
	t.Run("returns 413 for body too large", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/test", nil)

		handleDecodeError(rec, req, errBodyTooLarge)

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "too large")
	})

	t.Run("returns 400 for malformed JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/test", nil)

		handleDecodeError(rec, req, io.ErrUnexpectedEOF)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "invalid request body")
	})
}

// --- writeListJSON ---

func TestWriteListJSON(t *testing.T) {
	t.Run("writes list envelope with pagination", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		ctx := context.WithValue(req.Context(), contextKeyRequestID, "list-789")

		items := []string{"a", "b", "c"}
		total := 10
		writeListJSON(rec, req.WithContext(ctx), items, &total, true, 3, 0)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var resp model.ListResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotNil(t, resp.Total)
		assert.Equal(t, 10, *resp.Total)
		assert.True(t, resp.HasMore)
		assert.Equal(t, 3, resp.Limit)
		assert.Equal(t, 0, resp.Offset)
		assert.Equal(t, "list-789", resp.Meta.RequestID)
	})

	t.Run("writes nil total when access-filtered", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)

		writeListJSON(rec, req, []string{}, nil, false, 20, 0)

		var resp model.ListResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Nil(t, resp.Total)
		assert.False(t, resp.HasMore)
	})
}

// --- queryInt, queryLimit, queryOffset, queryTime ---

func TestQueryInt(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		key        string
		defaultVal int
		want       int
	}{
		{"present valid", "?count=5", "count", 10, 5},
		{"absent uses default", "?other=5", "count", 10, 10},
		{"invalid uses default", "?count=abc", "count", 10, 10},
		{"negative value", "?count=-3", "count", 10, -3},
		{"zero value", "?count=0", "count", 10, 0},
		{"empty value uses default", "?count=", "count", 10, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test"+tt.query, nil)
			got := queryInt(req, tt.key, tt.defaultVal)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestQueryLimit(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		defaultVal int
		want       int
	}{
		{"default when absent", "", 20, 20},
		{"respects explicit value", "?limit=50", 20, 50},
		{"clamps to 1 when zero", "?limit=0", 20, 1},
		{"clamps to 1 when negative", "?limit=-5", 20, 1},
		{"clamps to maxQueryLimit when too high", "?limit=9999", 20, maxQueryLimit},
		{"at maxQueryLimit boundary", "?limit=1000", 20, 1000},
		{"just above maxQueryLimit", "?limit=1001", 20, maxQueryLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test"+tt.query, nil)
			got := queryLimit(req, tt.defaultVal)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestQueryOffset(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"default when absent", "", 0},
		{"respects explicit value", "?offset=50", 50},
		{"clamps negative to 0", "?offset=-10", 0},
		{"clamps to maxQueryOffset", "?offset=200000", maxQueryOffset},
		{"at maxQueryOffset boundary", "?offset=100000", maxQueryOffset},
		{"just below maxQueryOffset", "?offset=99999", 99999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test"+tt.query, nil)
			got := queryOffset(req)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestQueryTime(t *testing.T) {
	t.Run("returns nil for absent key", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/test", nil)
		got, err := queryTime(req, "since")
		assert.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("parses valid RFC3339 time", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/test?since=2024-06-15T10:30:00Z", nil)
		got, err := queryTime(req, "since")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, 2024, got.Year())
		assert.Equal(t, time.Month(6), got.Month())
		assert.Equal(t, 15, got.Day())
	})

	t.Run("returns error for invalid time format", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/test?since=not-a-time", nil)
		got, err := queryTime(req, "since")
		assert.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "RFC3339")
	})

	t.Run("returns nil for empty value", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/test?since=", nil)
		got, err := queryTime(req, "since")
		assert.NoError(t, err)
		assert.Nil(t, got)
	})
}

// --- computePagination ---

func TestComputePagination(t *testing.T) {
	tests := []struct {
		name      string
		returned  int
		preFilter int
		limit     int
		offset    int
		dbTotal   int
		wantTotal *int
		wantMore  bool
	}{
		{
			name:     "exact page no filtering",
			returned: 10, preFilter: 10, limit: 10, offset: 0, dbTotal: 25,
			wantTotal: intPtr(25), wantMore: true,
		},
		{
			name:     "last page no filtering",
			returned: 5, preFilter: 5, limit: 10, offset: 20, dbTotal: 25,
			wantTotal: intPtr(25), wantMore: false,
		},
		{
			name:     "access-filtered partial page",
			returned: 3, preFilter: 10, limit: 10, offset: 0, dbTotal: 25,
			wantTotal: nil, wantMore: false,
		},
		{
			name:     "access-filtered full page",
			returned: 10, preFilter: 15, limit: 10, offset: 0, dbTotal: 50,
			wantTotal: nil, wantMore: true,
		},
		{
			name:     "empty result set",
			returned: 0, preFilter: 0, limit: 10, offset: 0, dbTotal: 0,
			wantTotal: intPtr(0), wantMore: false,
		},
		{
			name:     "single item exactly at limit",
			returned: 1, preFilter: 1, limit: 1, offset: 0, dbTotal: 3,
			wantTotal: intPtr(3), wantMore: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, hasMore := computePagination(tt.returned, tt.preFilter, tt.limit, tt.offset, tt.dbTotal)
			if tt.wantTotal == nil {
				assert.Nil(t, total)
			} else {
				require.NotNil(t, total)
				assert.Equal(t, *tt.wantTotal, *total)
			}
			assert.Equal(t, tt.wantMore, hasMore)
		})
	}
}

func intPtr(n int) *int { return &n }

// --- rateLimitMiddleware (additional coverage) ---

func TestRateLimitMiddleware(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter(1, 2)
	defer func() { _ = limiter.Close() }()

	logger := quietLogger()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	// Simulate 3 rapid requests from the same IP.
	for i := range 3 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/some-path", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		handler.ServeHTTP(rec, req)

		if i < 2 {
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: got status %d, want %d (within burst)", i+1, rec.Code, http.StatusOK)
			}
		} else {
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: got status %d, want %d (burst exhausted)", i+1, rec.Code, http.StatusTooManyRequests)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("rate-limited response should include Retry-After header")
			}
		}
	}
}

func TestRateLimitMiddleware_DifferentIPs(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter(1, 1)
	defer func() { _ = limiter.Close() }()

	logger := quietLogger()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	// First request from IP A should succeed.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("GET", "/path", nil)
	req1.RemoteAddr = "10.0.0.1:1000"
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Code)

	// Second request from IP A should be rate-limited (burst=1).
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/path", nil)
	req2.RemoteAddr = "10.0.0.1:1000"
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code)

	// First request from IP B should still succeed (separate bucket).
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/path", nil)
	req3.RemoteAddr = "10.0.0.2:1000"
	handler.ServeHTTP(rec3, req3)
	assert.Equal(t, http.StatusOK, rec3.Code)
}

func TestRateLimitMiddleware_PlatformAdminBypass(t *testing.T) {
	// Platform admins bypass rate limiting entirely.
	limiter := ratelimit.NewMemoryLimiter(1, 1)
	defer func() { _ = limiter.Close() }()

	logger := quietLogger()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	claims := &auth.Claims{
		AgentID: "superadmin",
		Role:    model.RolePlatformAdmin,
		OrgID:   uuid.New(),
	}

	// Send multiple requests; all should succeed because platform_admin bypasses rate limiting.
	for i := range 5 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.RemoteAddr = "10.0.0.1:1000"
		ctx := ctxutil.WithClaims(req.Context(), claims)
		handler.ServeHTTP(rec, req.WithContext(ctx))
		assert.Equal(t, http.StatusOK, rec.Code, "platform_admin request %d should succeed", i+1)
	}
}

func TestRateLimitMiddleware_AuthenticatedPerAgent(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter(1, 1)
	defer func() { _ = limiter.Close() }()

	logger := quietLogger()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	orgID := uuid.New()
	claimsA := &auth.Claims{AgentID: "agent-a", Role: model.RoleAgent, OrgID: orgID}
	claimsB := &auth.Claims{AgentID: "agent-b", Role: model.RoleAgent, OrgID: orgID}

	// Agent A first request: allowed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/agents", nil)
	ctx := ctxutil.WithClaims(req.Context(), claimsA)
	handler.ServeHTTP(rec, req.WithContext(ctx))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent A second request: rate-limited.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/agents", nil)
	ctx = ctxutil.WithClaims(req.Context(), claimsA)
	handler.ServeHTTP(rec, req.WithContext(ctx))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// Agent B first request: allowed (separate bucket).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/agents", nil)
	ctx = ctxutil.WithClaims(req.Context(), claimsB)
	handler.ServeHTTP(rec, req.WithContext(ctx))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRateLimitMiddleware_AuthenticatedPerAPIKey(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter(1, 1)
	defer func() { _ = limiter.Close() }()

	logger := quietLogger()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	orgID := uuid.New()
	keyID := uuid.New()
	claims := &auth.Claims{
		AgentID:  "agent-a",
		Role:     model.RoleAgent,
		OrgID:    orgID,
		APIKeyID: &keyID,
	}

	// First request: allowed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/agents", nil)
	ctx := ctxutil.WithClaims(req.Context(), claims)
	handler.ServeHTTP(rec, req.WithContext(ctx))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Second request: rate-limited (keyed on api key ID).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/agents", nil)
	ctx = ctxutil.WithClaims(req.Context(), claims)
	handler.ServeHTTP(rec, req.WithContext(ctx))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestRateLimitMiddleware_XForwardedFor(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter(1, 1)
	defer func() { _ = limiter.Close() }()

	logger := quietLogger()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// With trustProxy=true, rate limit key uses XFF client IP.
	handler := rateLimitMiddleware(limiter, logger, true, inner)

	// First request from client IP via XFF: allowed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/some-path", nil)
	req.RemoteAddr = "10.0.0.1:8080"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Second request from same client IP: rate-limited.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/some-path", nil)
	req.RemoteAddr = "10.0.0.1:8080"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// --- idempotencyKey ---

func TestIdempotencyKey(t *testing.T) {
	t.Run("returns trimmed header value", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Header.Set("Idempotency-Key", "  key-123  ")
		assert.Equal(t, "key-123", idempotencyKey(req))
	})

	t.Run("returns empty string when header absent", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		assert.Equal(t, "", idempotencyKey(req))
	})

	t.Run("returns empty string for whitespace-only header", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", nil)
		req.Header.Set("Idempotency-Key", "   ")
		assert.Equal(t, "", idempotencyKey(req))
	})
}

// --- requestHash ---

func TestRequestHash(t *testing.T) {
	t.Run("deterministic hash for same payload", func(t *testing.T) {
		payload := map[string]string{"key": "value"}
		h1, err1 := requestHash(payload)
		h2, err2 := requestHash(payload)
		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, h1, h2)
	})

	t.Run("different hash for different payload", func(t *testing.T) {
		h1, _ := requestHash(map[string]string{"key": "value1"})
		h2, _ := requestHash(map[string]string{"key": "value2"})
		assert.NotEqual(t, h1, h2)
	})

	t.Run("returns hex-encoded SHA256", func(t *testing.T) {
		h, err := requestHash("test")
		require.NoError(t, err)
		assert.Len(t, h, 64, "SHA256 hex digest should be 64 chars")
	})

	t.Run("handles nil payload", func(t *testing.T) {
		h, err := requestHash(nil)
		require.NoError(t, err)
		assert.NotEmpty(t, h)
	})
}

// --- parsePathUUID ---

func TestParsePathUUID(t *testing.T) {
	t.Run("parses valid UUID from path", func(t *testing.T) {
		expected := uuid.New()
		// Create a request served by a mux that extracts the path parameter.
		mux := http.NewServeMux()
		var got uuid.UUID
		var gotErr error
		mux.HandleFunc("GET /items/{item_id}", func(_ http.ResponseWriter, r *http.Request) {
			got, gotErr = parsePathUUID(r, "item_id")
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/items/"+expected.String(), nil)
		mux.ServeHTTP(rec, req)

		require.NoError(t, gotErr)
		assert.Equal(t, expected, got)
	})

	t.Run("returns error for invalid UUID", func(t *testing.T) {
		mux := http.NewServeMux()
		var gotErr error
		mux.HandleFunc("GET /items/{item_id}", func(_ http.ResponseWriter, r *http.Request) {
			_, gotErr = parsePathUUID(r, "item_id")
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/items/not-a-uuid", nil)
		mux.ServeHTTP(rec, req)

		assert.Error(t, gotErr)
	})

	t.Run("returns error for empty path value", func(t *testing.T) {
		// When there is no matching path param, PathValue returns "".
		req := httptest.NewRequest("GET", "/items/", nil)
		_, err := parsePathUUID(req, "item_id")
		assert.Error(t, err)
	})
}

// --- parseRunID ---

func TestParseRunID(t *testing.T) {
	t.Run("parses valid run_id", func(t *testing.T) {
		expected := uuid.New()
		mux := http.NewServeMux()
		var got uuid.UUID
		var gotErr error
		mux.HandleFunc("GET /v1/runs/{run_id}", func(_ http.ResponseWriter, r *http.Request) {
			got, gotErr = parseRunID(r)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/runs/"+expected.String(), nil)
		mux.ServeHTTP(rec, req)

		require.NoError(t, gotErr)
		assert.Equal(t, expected, got)
	})

	t.Run("returns error for missing run_id", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/runs/", nil)
		_, err := parseRunID(req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "run_id is required")
	})

	t.Run("returns error for invalid run_id", func(t *testing.T) {
		mux := http.NewServeMux()
		var gotErr error
		mux.HandleFunc("GET /v1/runs/{run_id}", func(_ http.ResponseWriter, r *http.Request) {
			_, gotErr = parseRunID(r)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/runs/invalid-uuid", nil)
		mux.ServeHTTP(rec, req)

		assert.Error(t, gotErr)
		assert.Contains(t, gotErr.Error(), "invalid run_id")
	})
}

// --- writeHookJSON ---

func TestWriteHookJSON(t *testing.T) {
	t.Run("writes hook response as JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeHookJSON(rec, hookResponse{
			Continue: true,
			HookSpecificOutput: &hookSpecific{
				HookEventName: "PostToolUse",
				Message:       "test message",
			},
		})

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue)
		require.NotNil(t, resp.HookSpecificOutput)
		assert.Equal(t, "PostToolUse", resp.HookSpecificOutput.HookEventName)
		assert.Equal(t, "test message", resp.HookSpecificOutput.Message)
	})

	t.Run("writes minimal hook response", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeHookJSON(rec, hookResponse{Continue: true, SuppressOutput: true})

		var resp hookResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Continue)
		assert.True(t, resp.SuppressOutput)
		assert.Nil(t, resp.HookSpecificOutput)
	})
}

// --- authMiddleware path filtering ---

func TestAuthMiddleware_PathFiltering(t *testing.T) {
	// authMiddleware requires a real JWTManager, but we can test the path filtering
	// logic by checking that non-authenticated paths pass through without auth.
	jwtMgr, _ := auth.NewJWTManager("", "", 24*time.Hour)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})

	handler := authMiddleware(jwtMgr, nil, inner)

	t.Run("unauthenticated paths pass through", func(t *testing.T) {
		paths := []string{"/health", "/", "/assets/main.js", "/config", "/auth/token"}
		for _, p := range paths {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", p, nil)
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code, "path %s should pass through without auth", p)
		}
	})

	t.Run("authenticated paths reject missing auth header", func(t *testing.T) {
		paths := []string{"/v1/agents", "/v1/decisions/recent", "/mcp"}
		for _, p := range paths {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", p, nil)
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "path %s should require auth", p)
		}
	})

	t.Run("rejects invalid authorization format", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Authorization", "malformed-no-space")
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("rejects unsupported auth scheme", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		var errResp model.APIError
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
		assert.Contains(t, errResp.Error.Message, "unsupported authorization scheme")
	})

	t.Run("rejects invalid bearer token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Authorization", "Bearer invalid-token-data")
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	// "accepts valid bearer token" is covered by integration tests in server_test.go.
	// Testing it here would require a non-nil *storage.DB because the middleware
	// fires a background goroutine to call TouchLastSeen.
}

// --- Additional decodeJSON edge case ---

func TestDecodeJSON_ExactlyAtLimit(t *testing.T) {
	// A body that is exactly at the limit should succeed.
	type small struct {
		A string `json:"a"`
	}
	body := `{"a":"b"}`
	rec := httptest.NewRecorder()
	req := &http.Request{Body: io.NopCloser(bytes.NewReader([]byte(body)))}

	var s small
	err := decodeJSON(rec, req, &s, int64(len(body)))
	require.NoError(t, err)
	assert.Equal(t, "b", s.A)
}
