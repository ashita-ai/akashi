package akashi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// tokenFuture represents an in-flight token refresh that multiple goroutines
// can wait on. This is the Go equivalent of the shared-promise pattern used
// by the TypeScript SDK.
type tokenFuture struct {
	done chan struct{}
	tok  string
	err  error
}

// tokenManager handles JWT token acquisition and refresh.
// It is safe for concurrent use. Concurrent callers share a single in-flight
// refresh rather than serializing behind the mutex during network I/O.
type tokenManager struct {
	baseURL string
	agentID string
	apiKey  string
	client  *http.Client
	margin  time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	inflight  *tokenFuture
}

func newTokenManager(baseURL, agentID, apiKey string, client *http.Client) *tokenManager {
	return &tokenManager{
		baseURL: baseURL,
		agentID: agentID,
		apiKey:  apiKey,
		client:  client,
		margin:  30 * time.Second,
	}
}

func (tm *tokenManager) getToken(ctx context.Context) (string, error) {
	tm.mu.Lock()

	// Fast path: token is still valid.
	if tm.token != "" && time.Now().Before(tm.expiresAt.Add(-tm.margin)) {
		tok := tm.token
		tm.mu.Unlock()
		return tok, nil
	}

	// A refresh is already in flight — wait on it without holding the mutex.
	if f := tm.inflight; f != nil {
		tm.mu.Unlock()
		return tm.awaitFuture(ctx, f)
	}

	// We are the first goroutine to see the expired token. Start a refresh
	// in a background goroutine so every caller — including this one — can
	// bail out via their own context without blocking the shared refresh.
	f := &tokenFuture{done: make(chan struct{})}
	tm.inflight = f
	tm.mu.Unlock()

	go tm.doRefresh(f)

	return tm.awaitFuture(ctx, f)
}

// doRefresh performs the HTTP token refresh and broadcasts the result to all
// waiters via the future's channel.
func (tm *tokenManager) doRefresh(f *tokenFuture) {
	// Use a background context so that one caller's cancellation doesn't
	// break the refresh for all waiters. Individual callers can still bail
	// out via their own ctx in awaitFuture.
	tok, expiresAt, err := tm.refresh(context.Background())

	tm.mu.Lock()
	if err == nil {
		tm.token = tok
		tm.expiresAt = expiresAt
	}
	f.tok = tok
	f.err = err
	tm.inflight = nil
	tm.mu.Unlock()

	close(f.done)
}

// awaitFuture waits for the in-flight refresh to complete, respecting the
// caller's context for cancellation/timeout.
func (tm *tokenManager) awaitFuture(ctx context.Context, f *tokenFuture) (string, error) {
	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		return "", fmt.Errorf("akashi: waiting for token refresh: %w", ctx.Err())
	}
}

type authRequest struct {
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
}

type authResponseEnvelope struct {
	Data struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"data"`
}

func (tm *tokenManager) refresh(ctx context.Context) (string, time.Time, error) {
	body, err := json.Marshal(authRequest{AgentID: tm.agentID, APIKey: tm.apiKey})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("akashi: marshal auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.baseURL+"/auth/token", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("akashi: create auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("akashi: auth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Read the error body so callers see the server's reason (e.g. "invalid api key")
		// rather than a generic status code.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var errEnv struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		var msg string
		if json.Unmarshal(body, &errEnv) == nil && errEnv.Error.Message != "" {
			msg = errEnv.Error.Message
		} else {
			msg = string(body)
			if msg == "" {
				msg = http.StatusText(resp.StatusCode)
			}
		}
		baseErr := fmt.Errorf("akashi: auth failed (%d): %s", resp.StatusCode, msg)
		if resp.StatusCode == http.StatusUnauthorized {
			return "", time.Time{}, &TokenExpiredError{Err: baseErr}
		}
		return "", time.Time{}, baseErr
	}

	var envelope authResponseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return "", time.Time{}, fmt.Errorf("akashi: decode auth response: %w", err)
	}

	return envelope.Data.Token, envelope.Data.ExpiresAt, nil
}
