package kyoyu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// tokenManager handles JWT token acquisition and refresh.
// It is safe for concurrent use.
type tokenManager struct {
	baseURL string
	agentID string
	apiKey  string
	client  *http.Client
	margin  time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time
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
	defer tm.mu.Unlock()

	if tm.token != "" && time.Now().Before(tm.expiresAt.Add(-tm.margin)) {
		return tm.token, nil
	}

	if err := tm.refresh(ctx); err != nil {
		return "", err
	}
	return tm.token, nil
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

func (tm *tokenManager) refresh(ctx context.Context) error {
	body, err := json.Marshal(authRequest{AgentID: tm.agentID, APIKey: tm.apiKey})
	if err != nil {
		return fmt.Errorf("kyoyu: marshal auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.baseURL+"/auth/token", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("kyoyu: create auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.client.Do(req)
	if err != nil {
		return fmt.Errorf("kyoyu: auth request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kyoyu: auth failed with status %d", resp.StatusCode)
	}

	var envelope authResponseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("kyoyu: decode auth response: %w", err)
	}

	tm.token = envelope.Data.Token
	tm.expiresAt = envelope.Data.ExpiresAt
	return nil
}
