// Package server implements the HTTP API server for Akashi.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
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
// Client-supplied IDs are accepted if they are reasonable length (≤128 chars)
// and contain only printable ASCII. Otherwise, a fresh UUID is generated.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if !isValidRequestID(reqID) {
			reqID = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), contextKeyRequestID, reqID)
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isValidRequestID checks that a client-supplied request ID is safe to log and echo.
func isValidRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < 0x20 || c > 0x7e { // reject control chars and non-ASCII
			return false
		}
	}
	return true
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
	tracer           = otel.Tracer("akashi/http")
	httpMeter        = otel.GetMeterProvider().Meter("akashi/http")
	httpRequestCount otelmetric.Int64Counter
	httpDuration     otelmetric.Float64Histogram
)

func init() {
	var err error
	httpRequestCount, err = httpMeter.Int64Counter("http.server.request_count")
	if err != nil {
		httpRequestCount, _ = httpMeter.Int64Counter("http.server.request_count.fallback")
	}
	httpDuration, err = httpMeter.Float64Histogram("http.server.duration",
		otelmetric.WithUnit("ms"))
	if err != nil {
		httpDuration, _ = httpMeter.Float64Histogram("http.server.duration.fallback",
			otelmetric.WithUnit("ms"))
	}
}

// routePattern extracts the registered mux pattern for metrics/spans.
// Falls back to method + first two path segments if the pattern is empty
// (e.g., for middleware-handled paths like /health that resolve before the mux).
func routePattern(r *http.Request) string {
	if pat := r.Pattern; pat != "" {
		return pat
	}
	// Fallback: use the first two path segments to bound cardinality.
	parts := strings.SplitN(r.URL.Path, "/", 4)
	if len(parts) >= 3 {
		return r.Method + " /" + parts[1] + "/" + parts[2]
	}
	return r.Method + " " + r.URL.Path
}

// tracingMiddleware creates an OTEL span for each HTTP request
// and records request count and duration metrics. The span name and
// metric labels use the mux route pattern (e.g., "GET /v1/runs/{run_id}")
// instead of the resolved URL path to avoid unbounded OTEL cardinality.
func tracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Start the span with a generic name; we update it after mux dispatch
		// once r.Pattern is populated.
		ctx, span := tracer.Start(r.Context(), "http.request",
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.Path),
				attribute.String("http.request_id", RequestIDFromContext(r.Context())),
			),
		)
		defer span.End()

		// Inject trace context into response headers so clients can correlate
		// their downstream operations with Akashi's server-side trace.
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(w.Header()))

		start := time.Now()

		// Reuse an existing statusWriter if the logging middleware already
		// wrapped the ResponseWriter (avoids one allocation per request).
		sw, ok := w.(*statusWriter)
		if !ok {
			sw = &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}
		}
		next.ServeHTTP(sw, r.WithContext(ctx))

		// After mux dispatch, r.Pattern is available.
		pattern := routePattern(r)
		span.SetName(pattern)

		duration := time.Since(start)
		statusStr := strconv.Itoa(sw.statusCode)

		span.SetAttributes(
			attribute.Int("http.status_code", sw.statusCode),
		)

		attrs := []attribute.KeyValue{
			attribute.String("http.method", r.Method),
			attribute.String("http.route", pattern),
			attribute.String("http.status_code", statusStr),
		}

		if claims := ClaimsFromContext(ctx); claims != nil {
			span.SetAttributes(
				attribute.String("akashi.agent_id", claims.AgentID),
				attribute.String("akashi.role", string(claims.Role)),
			)
			attrs = append(attrs, attribute.String("akashi.agent_id", claims.AgentID))
		}

		// Record metrics using pre-created instruments (no per-request allocation).
		httpRequestCount.Add(ctx, 1, otelmetric.WithAttributes(attrs...))
		httpDuration.Record(ctx, float64(duration.Milliseconds()), otelmetric.WithAttributes(attrs...))
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

// baggageMiddleware extracts the akashi.context_id OTEL baggage member (if present)
// and sets it as a span attribute. This enables cross-service correlation: a calling
// service can pass its Akashi run ID via OTEL baggage, and the span will include it.
func baggageMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bag := baggage.FromContext(r.Context())
		if member := bag.Member("akashi.context_id"); member.Value() != "" {
			span := trace.SpanFromContext(r.Context())
			span.SetAttributes(attribute.String("akashi.context_id", member.Value()))
		}
		next.ServeHTTP(w, r)
	})
}

// noAuthPaths are exact paths that skip JWT authentication entirely.
// WARNING: Every authenticated route prefix (/v1/, /mcp) MUST appear
// in the guard below. Adding a new prefix without updating the guard will silently
// bypass authentication.
var noAuthPaths = map[string]bool{
	"/auth/token":   true,
	"/auth/refresh": true,
	"/config":       true,
	"/health":       true,
	"/openapi.yaml": true,
}

// authMiddleware validates JWT tokens or API keys and populates context with claims.
// Uses an explicit allowlist of paths that skip auth. All paths under the
// authenticated prefixes (/v1/, /mcp) require valid credentials unless
// they appear in noAuthPaths.
//
// Supported authorization schemes:
//   - Bearer <jwt>           — standard JWT (fast, Ed25519 signature check)
//   - ApiKey <agent_id>:<key> — direct API key auth (Argon2 verify per request,
//     suitable for MCP clients and machine-to-machine integrations where token
//     refresh is impractical)
func authMiddleware(jwtMgr *auth.JWTManager, db *storage.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA static assets and client-side routes don't need auth.
		// Only paths under authenticated prefixes require JWT validation.
		// WARNING: Every authenticated route prefix MUST appear in this guard.
		if !strings.HasPrefix(r.URL.Path, "/v1/") &&
			!strings.HasPrefix(r.URL.Path, "/mcp") &&
			!noAuthPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		// Exact-match allowlist for public endpoints.
		if noAuthPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		// All other paths require valid credentials.
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "missing authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 {
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid authorization format")
			return
		}

		scheme := parts[0]
		credential := parts[1]

		var claims *auth.Claims

		switch {
		case strings.EqualFold(scheme, "Bearer"):
			var err error
			claims, err = jwtMgr.ValidateToken(credential)
			if err != nil {
				writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid or expired token")
				return
			}

		case strings.EqualFold(scheme, "ApiKey"):
			var err error
			claims, err = verifyAPIKey(r.Context(), db, credential)
			if err != nil {
				writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid api key")
				return
			}

		default:
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized,
				"unsupported authorization scheme (use Bearer or ApiKey)")
			return
		}

		ctx := ctxutil.WithClaims(r.Context(), claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// verifyAPIKey authenticates a request using "ApiKey agent_id:secret" credentials.
// Performs the same agent lookup + Argon2id verification as POST /auth/token.
// Returns synthesized claims on success; the claims are equivalent to what a JWT
// would contain but skip token issuance entirely.
func verifyAPIKey(ctx context.Context, db *storage.DB, credential string) (*auth.Claims, error) {
	// Parse "agent_id:api_key" — agent_ids cannot contain colons (validated on creation).
	colonIdx := strings.IndexByte(credential, ':')
	if colonIdx < 1 || colonIdx == len(credential)-1 {
		auth.DummyVerify()
		return nil, fmt.Errorf("invalid api key format")
	}
	agentID := credential[:colonIdx]
	apiKey := credential[colonIdx+1:]

	agents, err := db.GetAgentsByAgentIDGlobal(ctx, agentID)
	if err != nil {
		auth.DummyVerify()
		return nil, fmt.Errorf("invalid credentials")
	}

	verified := false
	for _, a := range agents {
		if a.APIKeyHash == nil {
			continue
		}
		valid, verr := auth.VerifyAPIKey(apiKey, *a.APIKeyHash)
		verified = true
		if verr != nil || !valid {
			continue
		}
		// Matched — synthesize claims equivalent to a JWT.
		return &auth.Claims{
			AgentID: a.AgentID,
			OrgID:   a.OrgID,
			Role:    a.Role,
		}, nil
	}

	if !verified {
		auth.DummyVerify()
	}
	return nil, fmt.Errorf("invalid credentials")
}

// requireRole returns middleware that enforces a minimum role level.
// Uses the role hierarchy: admin > agent > reader.
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

// corsMiddleware handles CORS preflight requests and sets response headers.
// Only origins listed in allowedOrigins are reflected. A single entry of "*"
// permits any origin (suitable for development or APIs using only bearer tokens).
func corsMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	// Pre-compute a set for O(1) lookup.
	originSet := make(map[string]bool, len(allowedOrigins))
	allowAll := false
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
			break
		}
		originSet[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowAll || originSet[origin]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
