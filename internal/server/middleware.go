// Package server implements the HTTP API server for Akashi.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
)

type contextKey string

const (
	contextKeyRequestID contextKey = "request_id"
)

// RequestIDFromContext extracts the request ID from the context.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// ClaimsFromContext extracts the JWT claims from the context.
// Delegates to ctxutil so MCP tools can use the same accessor.
func ClaimsFromContext(ctx context.Context) *auth.Claims {
	return ctxutil.ClaimsFromContext(ctx)
}

// OrgIDFromContext extracts the org_id from the context (set from JWT claims).
// Delegates to ctxutil so MCP tools can use the same accessor.
func OrgIDFromContext(ctx context.Context) uuid.UUID {
	return ctxutil.OrgIDFromContext(ctx)
}

// requestIDMiddleware assigns a unique request ID to each request.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), contextKeyRequestID, reqID)
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loggingMiddleware logs each request with structured fields.
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFromContext(r.Context()),
		}
		if tid := traceIDFromContext(r.Context()); tid != "" {
			attrs = append(attrs, "trace_id", tid)
		}
		if claims := ClaimsFromContext(r.Context()); claims != nil {
			attrs = append(attrs, "agent_id", claims.AgentID)
		}

		level := slog.LevelInfo
		if wrapped.statusCode >= 500 {
			level = slog.LevelError
		} else if wrapped.statusCode >= 400 {
			level = slog.LevelWarn
		}
		logger.Log(r.Context(), level, "http request", attrs...)
	})
}

type statusWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher so SSE works through the middleware chain.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter, enabling http.ResponseController
// and other Go 1.20+ features (Hijack, SetReadDeadline, etc.) to find it.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

var (
	tracer    = otel.Tracer("akashi/http")
	httpMeter = otel.GetMeterProvider().Meter("akashi/http")
)

// tracingMiddleware creates an OTEL span for each HTTP request
// and records request count and duration metrics.
// It reuses the statusWriter from loggingMiddleware if already present,
// avoiding a redundant wrapper allocation per request.
func tracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.Path),
				attribute.String("http.request_id", RequestIDFromContext(r.Context())),
			),
		)
		defer span.End()

		start := time.Now()

		// Reuse existing statusWriter if the logging middleware already wrapped w.
		sw, ok := w.(*statusWriter)
		if !ok {
			sw = &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}
		}
		next.ServeHTTP(sw, r.WithContext(ctx))

		duration := time.Since(start)
		statusStr := strconv.Itoa(sw.statusCode)

		span.SetAttributes(
			attribute.Int("http.status_code", sw.statusCode),
		)

		attrs := []attribute.KeyValue{
			attribute.String("http.method", r.Method),
			attribute.String("http.route", r.URL.Path),
			attribute.String("http.status_code", statusStr),
		}

		if claims := ClaimsFromContext(ctx); claims != nil {
			span.SetAttributes(
				attribute.String("akashi.agent_id", claims.AgentID),
				attribute.String("akashi.role", string(claims.Role)),
			)
			attrs = append(attrs, attribute.String("akashi.agent_id", claims.AgentID))
		}

		// Record metrics (best-effort, instruments lazily created).
		if counter, err := httpMeter.Int64Counter("http.server.request_count"); err == nil {
			counter.Add(ctx, 1, otelmetric.WithAttributes(attrs...))
		}
		if hist, err := httpMeter.Float64Histogram("http.server.duration",
			otelmetric.WithUnit("ms")); err == nil {
			hist.Record(ctx, float64(duration.Milliseconds()), otelmetric.WithAttributes(attrs...))
		}
	})
}

// traceIDFromContext extracts the OTEL trace ID from the context, if any.
func traceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// authMiddleware validates JWT tokens and populates context with claims.
// Skips auth for paths that don't require it (e.g., /health, /auth/token).
func authMiddleware(jwtMgr *auth.JWTManager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Paths that don't require auth.
		if r.URL.Path == "/health" || r.URL.Path == "/auth/token" ||
			r.URL.Path == "/auth/signup" || r.URL.Path == "/auth/verify" ||
			r.URL.Path == "/billing/webhooks" {
			next.ServeHTTP(w, r)
			return
		}

		// SPA static assets and client-side routes don't require auth.
		// Auth is enforced client-side and on the API endpoints.
		if !strings.HasPrefix(r.URL.Path, "/v1/") &&
			!strings.HasPrefix(r.URL.Path, "/billing/") &&
			!strings.HasPrefix(r.URL.Path, "/auth/") &&
			r.URL.Path != "/mcp" {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "missing authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid authorization format")
			return
		}

		claims, err := jwtMgr.ValidateToken(parts[1])
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid or expired token")
			return
		}

		ctx := ctxutil.WithClaims(r.Context(), claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole returns middleware that enforces a minimum role level.
// Uses the role hierarchy: platform_admin > org_owner > admin > agent > reader.
func requireRole(minRole model.AgentRole) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := ClaimsFromContext(r.Context())
			if claims == nil {
				writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "no claims in context")
				return
			}
			if !model.RoleAtLeast(claims.Role, minRole) {
				writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeJSON writes a JSON response with the standard envelope.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(model.APIResponse{
		Data: data,
		Meta: model.ResponseMeta{
			RequestID: RequestIDFromContext(r.Context()),
			Timestamp: time.Now().UTC(),
		},
	}); err != nil {
		slog.Warn("failed to encode JSON response",
			"error", err,
			"request_id", RequestIDFromContext(r.Context()))
	}
}

// writeError writes a JSON error response with the standard envelope.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(model.APIError{
		Error: model.ErrorDetail{Code: code, Message: message},
		Meta: model.ResponseMeta{
			RequestID: RequestIDFromContext(r.Context()),
			Timestamp: time.Now().UTC(),
		},
	}); err != nil {
		slog.Warn("failed to encode JSON error response",
			"error", err,
			"request_id", RequestIDFromContext(r.Context()))
	}
}

// writeInternalError logs the underlying error and writes a generic 500 response.
// This ensures every internal server error is visible in server logs for debugging,
// without leaking internal details to the client.
func (h *Handlers) writeInternalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	h.logger.Error(msg,
		"error", err,
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", RequestIDFromContext(r.Context()))
	writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, msg)
}

// recoveryMiddleware catches panics in downstream handlers, logs the stack trace,
// and returns a 500 error instead of crashing the server.
func recoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"error", rec,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware adds standard security response headers.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		next.ServeHTTP(w, r)
	})
}

// decodeJSON decodes a JSON request body into the target struct.
// Applies MaxBytesReader to prevent unbounded request bodies.
func decodeJSON(r *http.Request, target any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
