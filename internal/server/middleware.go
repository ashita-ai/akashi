// Package server implements the HTTP API server for Kyoyu.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/kyoyu/internal/auth"
	"github.com/ashita-ai/kyoyu/internal/model"
)

type contextKey string

const (
	contextKeyRequestID contextKey = "request_id"
	contextKeyClaims    contextKey = "claims"
)

// RequestIDFromContext extracts the request ID from the context.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// ClaimsFromContext extracts the JWT claims from the context.
func ClaimsFromContext(ctx context.Context) *auth.Claims {
	if v, ok := ctx.Value(contextKeyClaims).(*auth.Claims); ok {
		return v
	}
	return nil
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

var (
	tracer    = otel.Tracer("kyoyu/http")
	httpMeter = otel.GetMeterProvider().Meter("kyoyu/http")
)

// tracingMiddleware creates an OTEL span for each HTTP request
// and records request count and duration metrics.
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
		wrapped := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r.WithContext(ctx))

		duration := time.Since(start)
		statusStr := strconv.Itoa(wrapped.statusCode)

		span.SetAttributes(
			attribute.Int("http.status_code", wrapped.statusCode),
		)

		attrs := []attribute.KeyValue{
			attribute.String("http.method", r.Method),
			attribute.String("http.route", r.URL.Path),
			attribute.String("http.status_code", statusStr),
		}

		if claims := ClaimsFromContext(ctx); claims != nil {
			span.SetAttributes(
				attribute.String("kyoyu.agent_id", claims.AgentID),
				attribute.String("kyoyu.role", string(claims.Role)),
			)
			attrs = append(attrs, attribute.String("kyoyu.agent_id", claims.AgentID))
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
		if r.URL.Path == "/health" || r.URL.Path == "/auth/token" {
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

		ctx := context.WithValue(r.Context(), contextKeyClaims, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole returns middleware that enforces a minimum role.
func requireRole(roles ...model.AgentRole) func(http.Handler) http.Handler {
	roleSet := make(map[model.AgentRole]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := ClaimsFromContext(r.Context())
			if claims == nil {
				writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "no claims in context")
				return
			}
			if !roleSet[claims.Role] {
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
	_ = json.NewEncoder(w).Encode(model.APIResponse{
		Data: data,
		Meta: model.ResponseMeta{
			RequestID: RequestIDFromContext(r.Context()),
			Timestamp: time.Now().UTC(),
		},
	})
}

// writeError writes a JSON error response with the standard envelope.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(model.APIError{
		Error: model.ErrorDetail{Code: code, Message: message},
		Meta: model.ResponseMeta{
			RequestID: RequestIDFromContext(r.Context()),
			Timestamp: time.Now().UTC(),
		},
	})
}

// decodeJSON decodes a JSON request body into the target struct.
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
