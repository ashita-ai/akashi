package akashi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mockServer creates an httptest server that mimics the Akashi API.
func mockServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// Always register auth endpoint.
	if _, ok := handlers["POST /auth/token"]; !ok {
		mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"token":      "test-token-xyz",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				},
			})
		})
	}

	for pattern, handler := range handlers {
		mux.HandleFunc(pattern, handler)
	}

	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	return NewClient(Config{
		BaseURL: serverURL,
		AgentID: "test-agent",
		APIKey:  "test-key",
		Timeout: 5 * time.Second,
	})
}

func TestCheckReturnsPrecedents(t *testing.T) {
	decisionID := uuid.New()
	runID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/check": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-token-xyz" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"code": "UNAUTHORIZED", "message": "bad token"},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": CheckResponse{
					HasPrecedent: true,
					Decisions: []Decision{
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "other-agent",
							DecisionType: "deployment",
							Outcome:      "approved",
							Confidence:   0.95,
						},
					},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Check(context.Background(), CheckRequest{
		DecisionType: "deployment",
		Limit:        5,
	})
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !resp.HasPrecedent {
		t.Error("expected HasPrecedent to be true")
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(resp.Decisions))
	}
	if resp.Decisions[0].ID != decisionID {
		t.Errorf("expected decision ID %s, got %s", decisionID, resp.Decisions[0].ID)
	}
	if resp.Decisions[0].Outcome != "approved" {
		t.Errorf("expected outcome 'approved', got %q", resp.Decisions[0].Outcome)
	}
}

func TestTraceRecordsDecision(t *testing.T) {
	runID := uuid.New()
	decisionID := uuid.New()

	var receivedBody traceBody
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/trace": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"data": TraceResponse{
					RunID:      runID,
					DecisionID: decisionID,
					EventCount: 3,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	reasoning := "high confidence from prior data"
	resp, err := client.Trace(context.Background(), TraceRequest{
		DecisionType: "model_selection",
		Outcome:      "chose GPT-4",
		Confidence:   0.9,
		Reasoning:    &reasoning,
	})
	if err != nil {
		t.Fatalf("Trace failed: %v", err)
	}
	if resp.RunID != runID {
		t.Errorf("expected run_id %s, got %s", runID, resp.RunID)
	}
	if resp.DecisionID != decisionID {
		t.Errorf("expected decision_id %s, got %s", decisionID, resp.DecisionID)
	}
	if resp.EventCount != 3 {
		t.Errorf("expected event_count 3, got %d", resp.EventCount)
	}

	// Verify the SDK correctly wrapped the body for the server.
	if receivedBody.AgentID != "test-agent" {
		t.Errorf("expected agent_id 'test-agent', got %q", receivedBody.AgentID)
	}
	if receivedBody.Decision.DecisionType != "model_selection" {
		t.Errorf("expected decision_type 'model_selection', got %q", receivedBody.Decision.DecisionType)
	}
}

func TestTokenAutoRefreshOn401(t *testing.T) {
	var callCount atomic.Int32
	var authCount atomic.Int32

	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /auth/token": func(w http.ResponseWriter, r *http.Request) {
			n := authCount.Add(1)
			token := "token-v1"
			if n > 1 {
				token = "token-v2"
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"token": token,
					// Short expiry to force refresh.
					"expires_at": time.Now().Add(1 * time.Second).Format(time.RFC3339),
				},
			})
		},
		"POST /v1/check": func(w http.ResponseWriter, r *http.Request) {
			callCount.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"data": CheckResponse{HasPrecedent: false},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	// First call fetches a token.
	_, err := client.Check(context.Background(), CheckRequest{DecisionType: "test"})
	if err != nil {
		t.Fatalf("first check failed: %v", err)
	}
	if authCount.Load() != 1 {
		t.Errorf("expected 1 auth call, got %d", authCount.Load())
	}

	// Wait for the token to expire (past the 30s margin won't apply to 1s expiry).
	time.Sleep(1100 * time.Millisecond)

	// Second call should trigger a token refresh.
	_, err = client.Check(context.Background(), CheckRequest{DecisionType: "test"})
	if err != nil {
		t.Fatalf("second check failed: %v", err)
	}
	if authCount.Load() != 2 {
		t.Errorf("expected 2 auth calls after expiry, got %d", authCount.Load())
	}
}

func TestErrorTypesMapCorrectly(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       string
		message    string
		checkFn    func(error) bool
		checkLabel string
	}{
		{
			name: "404", status: http.StatusNotFound,
			code: "NOT_FOUND", message: "decision not found",
			checkFn: IsNotFound, checkLabel: "IsNotFound",
		},
		{
			name: "403", status: http.StatusForbidden,
			code: "FORBIDDEN", message: "no access",
			checkFn: IsForbidden, checkLabel: "IsForbidden",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockServer(t, map[string]http.HandlerFunc{
				"POST /v1/check": func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, tc.status, map[string]any{
						"error": map[string]any{
							"code":    tc.code,
							"message": tc.message,
						},
					})
				},
			})
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			_, err := client.Check(context.Background(), CheckRequest{DecisionType: "test"})
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			apiErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("expected *Error, got %T", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("expected status %d, got %d", tc.status, apiErr.StatusCode)
			}
			if apiErr.Code != tc.code {
				t.Errorf("expected code %q, got %q", tc.code, apiErr.Code)
			}
			if apiErr.Message != tc.message {
				t.Errorf("expected message %q, got %q", tc.message, apiErr.Message)
			}
			if !tc.checkFn(err) {
				t.Errorf("%s should return true", tc.checkLabel)
			}
		})
	}
}

func TestTimeoutHandling(t *testing.T) {
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/check": func(w http.ResponseWriter, r *http.Request) {
			// Simulate a slow server.
			time.Sleep(2 * time.Second)
			writeJSON(w, http.StatusOK, map[string]any{
				"data": CheckResponse{HasPrecedent: false},
			})
		},
	})
	defer srv.Close()

	client := NewClient(Config{
		BaseURL: srv.URL,
		AgentID: "test-agent",
		APIKey:  "test-key",
		Timeout: 100 * time.Millisecond, // Very short timeout.
	})

	_, err := client.Check(context.Background(), CheckRequest{DecisionType: "test"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
