package akashi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	c, err := NewClient(Config{
		BaseURL: serverURL,
		AgentID: "test-agent",
		APIKey:  "test-key",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Existing tests (Check, Trace, Token refresh, Error types, Timeout)
// ---------------------------------------------------------------------------

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
	var receivedHeaders http.Header
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/trace": func(w http.ResponseWriter, r *http.Request) {
			receivedHeaders = r.Header.Clone()
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

	// Verify User-Agent header.
	if got := receivedHeaders.Get("User-Agent"); got != "akashi-go/0.2.0" {
		t.Errorf("expected User-Agent 'akashi-go/0.2.0', got %q", got)
	}

	// Verify X-Akashi-Session header is a valid UUID.
	sessionStr := receivedHeaders.Get("X-Akashi-Session")
	if sessionStr == "" {
		t.Fatal("expected X-Akashi-Session header to be set")
	}
	if _, err := uuid.Parse(sessionStr); err != nil {
		t.Errorf("X-Akashi-Session %q is not a valid UUID: %v", sessionStr, err)
	}
}

func TestTraceWithContext(t *testing.T) {
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
					RunID:      uuid.New(),
					DecisionID: uuid.New(),
					EventCount: 1,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Trace(context.Background(), TraceRequest{
		DecisionType: "model_selection",
		Outcome:      "gpt-4o",
		Confidence:   0.9,
		Context: map[string]any{
			"model": "gpt-4o",
			"task":  "summarization",
			"repo":  "github.com/example/repo",
		},
	})
	if err != nil {
		t.Fatalf("Trace with context failed: %v", err)
	}

	if receivedBody.Context == nil {
		t.Fatal("expected context in wire body, got nil")
	}
	if receivedBody.Context["model"] != "gpt-4o" {
		t.Errorf("expected context.model 'gpt-4o', got %v", receivedBody.Context["model"])
	}
	if receivedBody.Context["task"] != "summarization" {
		t.Errorf("expected context.task 'summarization', got %v", receivedBody.Context["task"])
	}
	if receivedBody.Context["repo"] != "github.com/example/repo" {
		t.Errorf("expected context.repo 'github.com/example/repo', got %v", receivedBody.Context["repo"])
	}
}

func TestSessionIDOverride(t *testing.T) {
	fixedSession := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	var receivedSessionHeader string
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/trace": func(w http.ResponseWriter, r *http.Request) {
			receivedSessionHeader = r.Header.Get("X-Akashi-Session")
			writeJSON(w, http.StatusCreated, map[string]any{
				"data": TraceResponse{
					RunID:      uuid.New(),
					DecisionID: uuid.New(),
					EventCount: 1,
				},
			})
		},
	})
	defer srv.Close()

	client, err := NewClient(Config{
		BaseURL:   srv.URL,
		AgentID:   "test-agent",
		APIKey:    "test-key",
		SessionID: &fixedSession,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	_, err = client.Trace(context.Background(), TraceRequest{
		DecisionType: "test",
		Outcome:      "pass",
		Confidence:   1.0,
	})
	if err != nil {
		t.Fatalf("Trace failed: %v", err)
	}

	if receivedSessionHeader != fixedSession.String() {
		t.Errorf("expected session %s, got %q", fixedSession, receivedSessionHeader)
	}
}

func TestUserAgentOnAllRequests(t *testing.T) {
	var checkUA, getUA string
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/check": func(w http.ResponseWriter, r *http.Request) {
			checkUA = r.Header.Get("User-Agent")
			writeJSON(w, http.StatusOK, map[string]any{
				"data": CheckResponse{HasPrecedent: false},
			})
		},
		"GET /v1/decisions/recent": func(w http.ResponseWriter, r *http.Request) {
			getUA = r.Header.Get("User-Agent")
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{"decisions": []Decision{}},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _ = client.Check(context.Background(), CheckRequest{DecisionType: "test"})
	_, _ = client.Recent(context.Background(), nil)

	if checkUA != "akashi-go/0.2.0" {
		t.Errorf("Check: expected User-Agent 'akashi-go/0.2.0', got %q", checkUA)
	}
	if getUA != "akashi-go/0.2.0" {
		t.Errorf("Recent: expected User-Agent 'akashi-go/0.2.0', got %q", getUA)
	}
}

func TestSessionIDConsistentAcrossTraces(t *testing.T) {
	var sessions []string
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/trace": func(w http.ResponseWriter, r *http.Request) {
			sessions = append(sessions, r.Header.Get("X-Akashi-Session"))
			writeJSON(w, http.StatusCreated, map[string]any{
				"data": TraceResponse{
					RunID:      uuid.New(),
					DecisionID: uuid.New(),
					EventCount: 1,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	for range 3 {
		_, _ = client.Trace(context.Background(), TraceRequest{
			DecisionType: "test",
			Outcome:      "pass",
			Confidence:   1.0,
		})
	}

	if len(sessions) != 3 {
		t.Fatalf("expected 3 trace calls, got %d", len(sessions))
	}
	// All three should have the same session ID.
	if sessions[0] != sessions[1] || sessions[1] != sessions[2] {
		t.Errorf("expected consistent session IDs, got %v", sessions)
	}
	// And it should be a valid UUID.
	if _, err := uuid.Parse(sessions[0]); err != nil {
		t.Errorf("session ID %q is not a valid UUID: %v", sessions[0], err)
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
		{
			name: "429", status: http.StatusTooManyRequests,
			code: "RATE_LIMITED", message: "too many requests",
			checkFn: IsRateLimited, checkLabel: "IsRateLimited",
		},
		{
			name: "409", status: http.StatusConflict,
			code: "CONFLICT", message: "already exists",
			checkFn: IsConflict, checkLabel: "IsConflict",
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

	client, cErr := NewClient(Config{
		BaseURL: srv.URL,
		AgentID: "test-agent",
		APIKey:  "test-key",
		Timeout: 100 * time.Millisecond, // Very short timeout.
	})
	if cErr != nil {
		t.Fatalf("NewClient failed: %v", cErr)
	}

	_, err := client.Check(context.Background(), CheckRequest{DecisionType: "test"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Tests for Query, Search, Recent (previously untested)
// ---------------------------------------------------------------------------

func TestQueryReturnsDecisions(t *testing.T) {
	decisionID := uuid.New()
	runID := uuid.New()
	dt := "architecture"

	var receivedBody queryBody
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/query": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": QueryResponse{
					Decisions: []Decision{
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "planner",
							DecisionType: dt,
							Outcome:      "microservices",
							Confidence:   0.85,
						},
					},
					Total:  1,
					Limit:  50,
					Offset: 0,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Query(context.Background(), &QueryFilters{DecisionType: &dt}, nil)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(resp.Decisions))
	}
	if resp.Decisions[0].ID != decisionID {
		t.Errorf("expected decision ID %s, got %s", decisionID, resp.Decisions[0].ID)
	}

	// Verify body defaults were applied.
	if receivedBody.Limit != 50 {
		t.Errorf("expected default limit 50, got %d", receivedBody.Limit)
	}
	if receivedBody.OrderBy != "valid_from" {
		t.Errorf("expected default order_by 'valid_from', got %q", receivedBody.OrderBy)
	}
}

func TestSearchReturnsResults(t *testing.T) {
	decisionID := uuid.New()
	runID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/search": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			if body["query"] != "architecture decisions" {
				t.Errorf("expected query 'architecture decisions', got %v", body["query"])
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": SearchResponse{
					Results: []SearchResult{
						{
							Decision: Decision{
								ID:           decisionID,
								RunID:        runID,
								AgentID:      "planner",
								DecisionType: "architecture",
								Outcome:      "microservices",
								Confidence:   0.85,
							},
							SimilarityScore: 0.92,
						},
					},
					Total: 1,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Search(context.Background(), "architecture decisions", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].SimilarityScore != 0.92 {
		t.Errorf("expected similarity 0.92, got %f", resp.Results[0].SimilarityScore)
	}
}

func TestRecentReturnsDecisions(t *testing.T) {
	decisionID := uuid.New()
	runID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/decisions/recent": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("expected limit=5, got %q", r.URL.Query().Get("limit"))
			}
			if r.URL.Query().Get("agent_id") != "planner" {
				t.Errorf("expected agent_id=planner, got %q", r.URL.Query().Get("agent_id"))
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"decisions": []Decision{
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "planner",
							DecisionType: "routing",
							Outcome:      "route-a",
							Confidence:   0.88,
						},
					},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	decisions, err := client.Recent(context.Background(), &RecentOptions{
		Limit:   5,
		AgentID: "planner",
	})
	if err != nil {
		t.Fatalf("Recent failed: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	if decisions[0].ID != decisionID {
		t.Errorf("expected decision ID %s, got %s", decisionID, decisions[0].ID)
	}
}

// ---------------------------------------------------------------------------
// Tests for new run lifecycle endpoints
// ---------------------------------------------------------------------------

func TestCreateRun(t *testing.T) {
	runID := uuid.New()
	orgID := uuid.New()

	var receivedBody CreateRunRequest
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/runs": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"data": AgentRun{
					ID:        runID,
					AgentID:   receivedBody.AgentID,
					OrgID:     orgID,
					Status:    RunStatusRunning,
					StartedAt: time.Now(),
					CreatedAt: time.Now(),
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	traceID := "trace-abc"
	run, err := client.CreateRun(context.Background(), CreateRunRequest{
		AgentID: "test-agent",
		TraceID: &traceID,
		Metadata: map[string]any{
			"purpose": "testing",
		},
	})
	if err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}
	if run.ID != runID {
		t.Errorf("expected run ID %s, got %s", runID, run.ID)
	}
	if run.Status != RunStatusRunning {
		t.Errorf("expected status 'running', got %q", run.Status)
	}

	// Verify the request body was sent correctly.
	if receivedBody.AgentID != "test-agent" {
		t.Errorf("expected agent_id 'test-agent', got %q", receivedBody.AgentID)
	}
	if receivedBody.TraceID == nil || *receivedBody.TraceID != "trace-abc" {
		t.Errorf("expected trace_id 'trace-abc', got %v", receivedBody.TraceID)
	}
}

func TestAppendEvents(t *testing.T) {
	runID := uuid.New()
	eventID1 := uuid.New()
	eventID2 := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/runs/" + runID.String() + "/events": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			events, ok := body["events"].([]any)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": "events required"},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"accepted":  len(events),
					"event_ids": []uuid.UUID{eventID1, eventID2},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.AppendEvents(context.Background(), runID, []EventInput{
		{
			EventType: EventDecisionStarted,
			Payload:   map[string]any{"decision_type": "model_selection"},
		},
		{
			EventType: EventDecisionMade,
			Payload:   map[string]any{"outcome": "gpt-4"},
		},
	})
	if err != nil {
		t.Fatalf("AppendEvents failed: %v", err)
	}
	if resp.Accepted != 2 {
		t.Errorf("expected 2 accepted events, got %d", resp.Accepted)
	}
	if len(resp.EventIDs) != 2 {
		t.Errorf("expected 2 event IDs, got %d", len(resp.EventIDs))
	}
}

func TestCompleteRun(t *testing.T) {
	runID := uuid.New()
	orgID := uuid.New()

	var receivedBody CompleteRunRequest
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/runs/" + runID.String() + "/complete": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			now := time.Now()
			writeJSON(w, http.StatusOK, map[string]any{
				"data": AgentRun{
					ID:          runID,
					AgentID:     "test-agent",
					OrgID:       orgID,
					Status:      RunStatus(receivedBody.Status),
					StartedAt:   now.Add(-1 * time.Minute),
					CompletedAt: &now,
					Metadata:    receivedBody.Metadata,
					CreatedAt:   now.Add(-1 * time.Minute),
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	meta := map[string]any{"exit_code": float64(0)}
	run, err := client.CompleteRun(context.Background(), runID, "completed", meta)
	if err != nil {
		t.Fatalf("CompleteRun failed: %v", err)
	}
	if run.Status != RunStatusCompleted {
		t.Errorf("expected status 'completed', got %q", run.Status)
	}
	if run.CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}

	// Verify the request body.
	if receivedBody.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", receivedBody.Status)
	}
}

func TestGetRun(t *testing.T) {
	runID := uuid.New()
	orgID := uuid.New()
	eventID := uuid.New()
	decisionID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/runs/" + runID.String(): func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-token-xyz" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"code": "UNAUTHORIZED", "message": "bad token"},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"run": AgentRun{
						ID:        runID,
						AgentID:   "test-agent",
						OrgID:     orgID,
						Status:    RunStatusRunning,
						StartedAt: now,
						CreatedAt: now,
					},
					"events": []AgentEvent{
						{
							ID:          eventID,
							RunID:       runID,
							EventType:   EventDecisionMade,
							SequenceNum: 1,
							OccurredAt:  now,
							AgentID:     "test-agent",
							Payload:     map[string]any{"outcome": "test"},
							CreatedAt:   now,
						},
					},
					"decisions": []Decision{
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "test-agent",
							DecisionType: "test",
							Outcome:      "pass",
							Confidence:   0.99,
							CreatedAt:    now,
						},
					},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if resp.Run.ID != runID {
		t.Errorf("expected run ID %s, got %s", runID, resp.Run.ID)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(resp.Events))
	}
	if resp.Events[0].ID != eventID {
		t.Errorf("expected event ID %s, got %s", eventID, resp.Events[0].ID)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(resp.Decisions))
	}
	if resp.Decisions[0].ID != decisionID {
		t.Errorf("expected decision ID %s, got %s", decisionID, resp.Decisions[0].ID)
	}
}

// ---------------------------------------------------------------------------
// Tests for agent management endpoints
// ---------------------------------------------------------------------------

func TestCreateAgent(t *testing.T) {
	agentUUID := uuid.New()
	orgID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	var receivedBody CreateAgentRequest
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/agents": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"data": Agent{
					ID:        agentUUID,
					AgentID:   receivedBody.AgentID,
					OrgID:     orgID,
					Name:      receivedBody.Name,
					Role:      receivedBody.Role,
					Metadata:  receivedBody.Metadata,
					CreatedAt: now,
					UpdatedAt: now,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	agent, err := client.CreateAgent(context.Background(), CreateAgentRequest{
		AgentID:  "new-agent",
		Name:     "New Agent",
		Role:     RoleAgent,
		APIKey:   "super-secret-key",
		Metadata: map[string]any{"team": "backend"},
	})
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}
	if agent.ID != agentUUID {
		t.Errorf("expected agent UUID %s, got %s", agentUUID, agent.ID)
	}
	if agent.AgentID != "new-agent" {
		t.Errorf("expected agent_id 'new-agent', got %q", agent.AgentID)
	}
	if agent.Role != RoleAgent {
		t.Errorf("expected role 'agent', got %q", agent.Role)
	}

	// Verify request body was sent correctly.
	if receivedBody.AgentID != "new-agent" {
		t.Errorf("expected agent_id 'new-agent' in body, got %q", receivedBody.AgentID)
	}
	if receivedBody.APIKey != "super-secret-key" {
		t.Errorf("expected api_key in body, got %q", receivedBody.APIKey)
	}
}

func TestListAgents(t *testing.T) {
	agentUUID := uuid.New()
	orgID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/agents": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": []Agent{
					{
						ID:        agentUUID,
						AgentID:   "admin",
						OrgID:     orgID,
						Name:      "System Admin",
						Role:      RoleAdmin,
						CreatedAt: now,
						UpdatedAt: now,
					},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	agents, err := client.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents failed: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].AgentID != "admin" {
		t.Errorf("expected agent_id 'admin', got %q", agents[0].AgentID)
	}
	if agents[0].Role != RoleAdmin {
		t.Errorf("expected role 'admin', got %q", agents[0].Role)
	}
}

func TestDeleteAgent(t *testing.T) {
	srv := mockServer(t, map[string]http.HandlerFunc{
		"DELETE /v1/agents/old-agent": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE method, got %s", r.Method)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"agent_id": "old-agent",
					"deleted": map[string]any{
						"events":    float64(42),
						"decisions": float64(5),
						"runs":      float64(3),
					},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.DeleteAgent(context.Background(), "old-agent")
	if err != nil {
		t.Fatalf("DeleteAgent failed: %v", err)
	}
	if resp.AgentID != "old-agent" {
		t.Errorf("expected agent_id 'old-agent', got %q", resp.AgentID)
	}
}

// ---------------------------------------------------------------------------
// Tests for temporal queries
// ---------------------------------------------------------------------------

func TestTemporalQuery(t *testing.T) {
	decisionID := uuid.New()
	runID := uuid.New()
	asOf := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	var receivedBody TemporalQueryRequest
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/query/temporal": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"as_of": asOf,
					"decisions": []Decision{
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "planner",
							DecisionType: "architecture",
							Outcome:      "monolith",
							Confidence:   0.7,
						},
					},
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	dt := "architecture"
	resp, err := client.TemporalQuery(context.Background(), asOf, &QueryFilters{
		DecisionType: &dt,
	})
	if err != nil {
		t.Fatalf("TemporalQuery failed: %v", err)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(resp.Decisions))
	}
	if resp.Decisions[0].Outcome != "monolith" {
		t.Errorf("expected outcome 'monolith', got %q", resp.Decisions[0].Outcome)
	}

	// Verify the request body was constructed correctly.
	if receivedBody.AsOf.IsZero() {
		t.Error("expected as_of to be set in request body")
	}
	if receivedBody.Filters.DecisionType == nil || *receivedBody.Filters.DecisionType != "architecture" {
		t.Errorf("expected decision_type filter 'architecture', got %v", receivedBody.Filters.DecisionType)
	}
}

func TestAgentHistory(t *testing.T) {
	decisionID := uuid.New()
	runID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/agents/planner/history": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("limit") != "10" {
				t.Errorf("expected limit=10, got %q", r.URL.Query().Get("limit"))
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"agent_id": "planner",
					"decisions": []Decision{
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "planner",
							DecisionType: "routing",
							Outcome:      "route-b",
							Confidence:   0.75,
						},
					},
					"total":  1,
					"limit":  10,
					"offset": 0,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.AgentHistory(context.Background(), "planner", 10)
	if err != nil {
		t.Fatalf("AgentHistory failed: %v", err)
	}
	if resp.AgentID != "planner" {
		t.Errorf("expected agent_id 'planner', got %q", resp.AgentID)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(resp.Decisions))
	}
}

// ---------------------------------------------------------------------------
// Tests for grant management
// ---------------------------------------------------------------------------

func TestCreateGrant(t *testing.T) {
	grantID := uuid.New()
	orgID := uuid.New()
	grantorID := uuid.New()
	granteeID := uuid.New()

	var receivedBody CreateGrantRequest
	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/grants": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			now := time.Now()
			writeJSON(w, http.StatusCreated, map[string]any{
				"data": Grant{
					ID:           grantID,
					OrgID:        orgID,
					GrantorID:    grantorID,
					GranteeID:    granteeID,
					ResourceType: receivedBody.ResourceType,
					ResourceID:   receivedBody.ResourceID,
					Permission:   receivedBody.Permission,
					GrantedAt:    now,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resID := "planner"
	grant, err := client.CreateGrant(context.Background(), CreateGrantRequest{
		GranteeAgentID: "reader-agent",
		ResourceType:   "agent_traces",
		ResourceID:     &resID,
		Permission:     "read",
	})
	if err != nil {
		t.Fatalf("CreateGrant failed: %v", err)
	}
	if grant.ID != grantID {
		t.Errorf("expected grant ID %s, got %s", grantID, grant.ID)
	}
	if grant.Permission != "read" {
		t.Errorf("expected permission 'read', got %q", grant.Permission)
	}

	// Verify request body.
	if receivedBody.GranteeAgentID != "reader-agent" {
		t.Errorf("expected grantee_agent_id 'reader-agent', got %q", receivedBody.GranteeAgentID)
	}
	if receivedBody.ResourceType != "agent_traces" {
		t.Errorf("expected resource_type 'agent_traces', got %q", receivedBody.ResourceType)
	}
}

func TestDeleteGrant(t *testing.T) {
	grantID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"DELETE /v1/grants/" + grantID.String(): func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE method, got %s", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.DeleteGrant(context.Background(), grantID)
	if err != nil {
		t.Fatalf("DeleteGrant failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests for conflicts, usage, and health
// ---------------------------------------------------------------------------

func TestListConflicts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/conflicts": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("decision_type") != "architecture" {
				t.Errorf("expected decision_type=architecture, got %q", r.URL.Query().Get("decision_type"))
			}
			if r.URL.Query().Get("limit") != "10" {
				t.Errorf("expected limit=10, got %q", r.URL.Query().Get("limit"))
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"conflicts": []DecisionConflict{
						{
							ConflictKind: ConflictKindCrossAgent,
							DecisionAID:  uuid.New(),
							DecisionBID:  uuid.New(),
							AgentA:       "planner",
							AgentB:       "coder",
							RunA:         uuid.New(),
							RunB:         uuid.New(),
							DecisionType: "architecture",
							OutcomeA:     "microservices",
							OutcomeB:     "monolith",
							ConfidenceA:  0.85,
							ConfidenceB:  0.90,
							DecidedAtA:   now,
							DecidedAtB:   now,
							DetectedAt:   now,
						},
					},
					"total":  1,
					"limit":  10,
					"offset": 0,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.ListConflicts(context.Background(), &ConflictOptions{
		DecisionType: "architecture",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListConflicts failed: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
	if len(resp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(resp.Conflicts))
	}
	if resp.Conflicts[0].OutcomeA != "microservices" {
		t.Errorf("expected outcome_a 'microservices', got %q", resp.Conflicts[0].OutcomeA)
	}
	if resp.Conflicts[0].OutcomeB != "monolith" {
		t.Errorf("expected outcome_b 'monolith', got %q", resp.Conflicts[0].OutcomeB)
	}
}

func TestListConflictsNilOptions(t *testing.T) {
	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/conflicts": func(w http.ResponseWriter, r *http.Request) {
			// Verify no query parameters were sent.
			if r.URL.RawQuery != "" {
				t.Errorf("expected no query params, got %q", r.URL.RawQuery)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"conflicts": []DecisionConflict{},
					"total":     0,
					"limit":     25,
					"offset":    0,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.ListConflicts(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListConflicts with nil opts failed: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("expected total 0, got %d", resp.Total)
	}
}

func TestHealth(t *testing.T) {
	// Health endpoint should work without auth.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// Verify no Authorization header is sent.
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header, got %q", auth)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": HealthResponse{
				Status:        "healthy",
				Version:       "v0.1.0",
				Postgres:      "connected",
				Qdrant:        "connected",
				UptimeSeconds: 3600,
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Intentionally use bad credentials to prove health doesn't need auth.
	client, cErr := NewClient(Config{
		BaseURL: srv.URL,
		AgentID: "bad-agent",
		APIKey:  "bad-key",
		Timeout: 5 * time.Second,
	})
	if cErr != nil {
		t.Fatalf("NewClient failed: %v", cErr)
	}

	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if health.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", health.Status)
	}
	if health.Version != "v0.1.0" {
		t.Errorf("expected version 'v0.1.0', got %q", health.Version)
	}
	if health.Postgres != "connected" {
		t.Errorf("expected postgres 'connected', got %q", health.Postgres)
	}
	if health.Qdrant != "connected" {
		t.Errorf("expected qdrant 'connected', got %q", health.Qdrant)
	}
	if health.UptimeSeconds != 3600 {
		t.Errorf("expected uptime_seconds 3600, got %d", health.UptimeSeconds)
	}
}

func TestHealthNoAuth(t *testing.T) {
	// Ensure the Health endpoint does NOT call /auth/token.
	var authCalled atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
		authCalled.Store(true)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"code": "UNAUTHORIZED", "message": "bad key"},
		})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": HealthResponse{
				Status:        "healthy",
				Version:       "v0.1.0",
				Postgres:      "connected",
				UptimeSeconds: 100,
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, cErr := NewClient(Config{
		BaseURL: srv.URL,
		AgentID: "test",
		APIKey:  "test",
		Timeout: 5 * time.Second,
	})
	if cErr != nil {
		t.Fatalf("NewClient failed: %v", cErr)
	}

	_, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if authCalled.Load() {
		t.Error("Health endpoint should not trigger auth token request")
	}
}

// ---------------------------------------------------------------------------
// Test error helpers
// ---------------------------------------------------------------------------

func TestIsRateLimited(t *testing.T) {
	err := &Error{StatusCode: 429, Code: "RATE_LIMITED", Message: "slow down"}
	if !IsRateLimited(err) {
		t.Error("IsRateLimited should return true for 429")
	}
	if IsRateLimited(&Error{StatusCode: 200}) {
		t.Error("IsRateLimited should return false for 200")
	}
	if IsRateLimited(nil) {
		t.Error("IsRateLimited should return false for nil")
	}
}

func TestIsConflict(t *testing.T) {
	err := &Error{StatusCode: 409, Code: "CONFLICT", Message: "already exists"}
	if !IsConflict(err) {
		t.Error("IsConflict should return true for 409")
	}
	if IsConflict(&Error{StatusCode: 200}) {
		t.Error("IsConflict should return false for 200")
	}
}

// ---------------------------------------------------------------------------
// NewClient validation (SDK3)
// ---------------------------------------------------------------------------

func TestNewClientValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "empty BaseURL",
			cfg:     Config{AgentID: "a", APIKey: "k"},
			wantErr: "BaseURL is required",
		},
		{
			name:    "empty AgentID",
			cfg:     Config{BaseURL: "http://localhost:8080", APIKey: "k"},
			wantErr: "AgentID is required",
		},
		{
			name:    "empty APIKey",
			cfg:     Config{BaseURL: "http://localhost:8080", AgentID: "a"},
			wantErr: "APIKey is required",
		},
		{
			name: "all empty",
			cfg:  Config{},
			// First check is BaseURL.
			wantErr: "BaseURL is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(tc.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if c != nil {
				t.Error("expected nil client on error")
			}
			if got := err.Error(); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error %q does not contain %q", got, tc.wantErr)
			}
		})
	}

	// Happy path.
	c, err := NewClient(Config{
		BaseURL: "http://localhost:8080/",
		AgentID: "test",
		APIKey:  "key",
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// ---------------------------------------------------------------------------
// Tests for new endpoint methods (SDK2)
// ---------------------------------------------------------------------------

func TestGetDecisionRevisions(t *testing.T) {
	decisionID := uuid.New()
	orgID := uuid.New()
	runID := uuid.New()
	supersededID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/decisions/" + decisionID.String() + "/revisions": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"decision_id": decisionID,
					"revisions": []Decision{
						{
							ID:           supersededID,
							RunID:        runID,
							AgentID:      "planner",
							OrgID:        orgID,
							DecisionType: "architecture",
							Outcome:      "monolith",
							Confidence:   0.7,
							CreatedAt:    now.Add(-1 * time.Hour),
						},
						{
							ID:           decisionID,
							RunID:        runID,
							AgentID:      "planner",
							OrgID:        orgID,
							DecisionType: "architecture",
							Outcome:      "microservices",
							Confidence:   0.85,
							SupersedesID: &supersededID,
							CreatedAt:    now,
						},
					},
					"count": 2,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.GetDecisionRevisions(context.Background(), decisionID)
	if err != nil {
		t.Fatalf("GetDecisionRevisions failed: %v", err)
	}
	if resp.DecisionID != decisionID {
		t.Errorf("expected decision_id %s, got %s", decisionID, resp.DecisionID)
	}
	if resp.Count != 2 {
		t.Errorf("expected count 2, got %d", resp.Count)
	}
	if len(resp.Revisions) != 2 {
		t.Fatalf("expected 2 revisions, got %d", len(resp.Revisions))
	}
	if resp.Revisions[0].Outcome != "monolith" {
		t.Errorf("expected first revision outcome 'monolith', got %q", resp.Revisions[0].Outcome)
	}
	if resp.Revisions[1].SupersedesID == nil || *resp.Revisions[1].SupersedesID != supersededID {
		t.Errorf("expected second revision to supersede %s", supersededID)
	}
}

func TestVerifyDecision(t *testing.T) {
	decisionID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"GET /v1/verify/" + decisionID.String(): func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"decision_id":   decisionID,
					"valid":         true,
					"stored_hash":   "sha256:abc123",
					"computed_hash": "sha256:abc123",
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.VerifyDecision(context.Background(), decisionID)
	if err != nil {
		t.Fatalf("VerifyDecision failed: %v", err)
	}
	if resp.DecisionID != decisionID {
		t.Errorf("expected decision_id %s, got %s", decisionID, resp.DecisionID)
	}
	if !resp.Valid {
		t.Error("expected valid to be true")
	}
	if resp.StoredHash != "sha256:abc123" {
		t.Errorf("expected stored_hash 'sha256:abc123', got %q", resp.StoredHash)
	}
	if resp.ComputedHash != "sha256:abc123" {
		t.Errorf("expected computed_hash 'sha256:abc123', got %q", resp.ComputedHash)
	}
}

func TestUpdateAgentTags(t *testing.T) {
	agentUUID := uuid.New()
	orgID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	var receivedBody map[string]any
	srv := mockServer(t, map[string]http.HandlerFunc{
		"PATCH /v1/agents/planner/tags": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("expected PATCH method, got %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"code": "INVALID_INPUT", "message": err.Error()},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": Agent{
					ID:        agentUUID,
					AgentID:   "planner",
					OrgID:     orgID,
					Name:      "Planner",
					Role:      RoleAgent,
					Tags:      []string{"backend", "infra"},
					CreatedAt: now,
					UpdatedAt: now,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	agent, err := client.UpdateAgentTags(context.Background(), "planner", []string{"backend", "infra"})
	if err != nil {
		t.Fatalf("UpdateAgentTags failed: %v", err)
	}
	if agent.AgentID != "planner" {
		t.Errorf("expected agent_id 'planner', got %q", agent.AgentID)
	}
	if len(agent.Tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(agent.Tags))
	}
	if agent.Tags[0] != "backend" || agent.Tags[1] != "infra" {
		t.Errorf("expected tags [backend, infra], got %v", agent.Tags)
	}

	// Verify request body was sent correctly.
	tags, ok := receivedBody["tags"].([]any)
	if !ok {
		t.Fatal("expected tags in request body")
	}
	if len(tags) != 2 {
		t.Errorf("expected 2 tags in body, got %d", len(tags))
	}
}

// ---------------------------------------------------------------------------
// Test deserialization of new fields (SDK1)
// ---------------------------------------------------------------------------

func TestDecisionDeserializesAllFields(t *testing.T) {
	orgID := uuid.New()
	decisionID := uuid.New()
	runID := uuid.New()
	precedentRef := uuid.New()
	supersedesID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/query": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"decisions": []map[string]any{
						{
							"id":               decisionID,
							"run_id":            runID,
							"agent_id":          "planner",
							"org_id":            orgID,
							"decision_type":     "architecture",
							"outcome":           "microservices",
							"confidence":        0.85,
							"metadata":          map[string]any{},
							"quality_score":     0.92,
							"precedent_ref":     precedentRef,
							"supersedes_id":     supersedesID,
							"content_hash":      "sha256:abc123def456",
							"tags":              []string{"backend", "infra"},
							"valid_from":        now,
							"transaction_time":  now,
							"created_at":        now,
						},
					},
					"total":  1,
					"count":  1,
					"limit":  50,
					"offset": 0,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	dt := "architecture"
	resp, err := client.Query(context.Background(), &QueryFilters{DecisionType: &dt}, nil)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if resp.Count != 1 {
		t.Errorf("expected count 1, got %d", resp.Count)
	}

	d := resp.Decisions[0]
	if d.OrgID != orgID {
		t.Errorf("expected org_id %s, got %s", orgID, d.OrgID)
	}
	if d.QualityScore != 0.92 {
		t.Errorf("expected quality_score 0.92, got %f", d.QualityScore)
	}
	if d.PrecedentRef == nil || *d.PrecedentRef != precedentRef {
		t.Errorf("expected precedent_ref %s, got %v", precedentRef, d.PrecedentRef)
	}
	if d.SupersedesID == nil || *d.SupersedesID != supersedesID {
		t.Errorf("expected supersedes_id %s, got %v", supersedesID, d.SupersedesID)
	}
	if d.ContentHash != "sha256:abc123def456" {
		t.Errorf("expected content_hash 'sha256:abc123def456', got %q", d.ContentHash)
	}
	if len(d.Tags) != 2 || d.Tags[0] != "backend" || d.Tags[1] != "infra" {
		t.Errorf("expected tags [backend, infra], got %v", d.Tags)
	}
}

func TestDecisionDeserializesSpec31Fields(t *testing.T) {
	sessionID := uuid.New()

	srv := mockServer(t, map[string]http.HandlerFunc{
		"POST /v1/query": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"decisions": []map[string]any{
						{
							"id":            uuid.New(),
							"run_id":        uuid.New(),
							"agent_id":      "coder",
							"org_id":        uuid.New(),
							"decision_type": "architecture",
							"outcome":       "microservices",
							"confidence":    0.85,
							"metadata":      map[string]any{},
							"session_id":    sessionID,
							"agent_context": map[string]any{
								"tool":         "claude-code",
								"tool_version": "akashi-go/0.2.0",
								"model":        "claude-opus-4-6",
								"task":         "code review",
							},
							"valid_from":       time.Now(),
							"transaction_time": time.Now(),
							"created_at":       time.Now(),
						},
					},
					"total":  1,
					"count":  1,
					"limit":  50,
					"offset": 0,
				},
			})
		},
	})
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	dt := "architecture"
	resp, err := client.Query(context.Background(), &QueryFilters{DecisionType: &dt}, nil)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	d := resp.Decisions[0]
	if d.SessionID == nil || *d.SessionID != sessionID {
		t.Errorf("expected session_id %s, got %v", sessionID, d.SessionID)
	}
	if d.AgentContext == nil {
		t.Fatal("expected agent_context to be non-nil")
	}
	if d.AgentContext["tool"] != "claude-code" {
		t.Errorf("expected agent_context.tool 'claude-code', got %v", d.AgentContext["tool"])
	}
	if d.AgentContext["model"] != "claude-opus-4-6" {
		t.Errorf("expected agent_context.model 'claude-opus-4-6', got %v", d.AgentContext["model"])
	}
}
