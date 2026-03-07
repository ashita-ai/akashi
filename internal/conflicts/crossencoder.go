package conflicts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// crossEncoderTimeout is the per-call timeout for cross-encoder scoring.
// Cross-encoders are fast (10-50ms per pair on GPU), so 5s is generous
// and accounts for network latency and cold starts.
const crossEncoderTimeout = 5 * time.Second

// CrossEncoder scores a pair of texts for contradiction likelihood.
// Returns a score between 0 (no contradiction) and 1 (definite contradiction).
type CrossEncoder interface {
	ScoreContradiction(ctx context.Context, textA, textB string) (float64, error)
}

// HTTPCrossEncoder calls an external cross-encoder service via HTTP.
// The service must expose POST {url}/score accepting {"text_a": "...", "text_b": "..."}
// and returning {"score": 0.0-1.0}.
type HTTPCrossEncoder struct {
	url        string
	httpClient *http.Client
}

// NewHTTPCrossEncoder creates a cross-encoder client pointing at the given base URL.
// The client will POST to {url}/score.
func NewHTTPCrossEncoder(url string) *HTTPCrossEncoder {
	return &HTTPCrossEncoder{
		url: url,
		httpClient: &http.Client{
			Timeout: crossEncoderTimeout + 2*time.Second,
		},
	}
}

type crossEncoderRequest struct {
	TextA string `json:"text_a"`
	TextB string `json:"text_b"`
}

type crossEncoderResponse struct {
	Score float64 `json:"score"`
}

// ScoreContradiction sends a pair of texts to the cross-encoder service and
// returns the contradiction likelihood score (0-1).
func (c *HTTPCrossEncoder) ScoreContradiction(ctx context.Context, textA, textB string) (float64, error) {
	callCtx, cancel := context.WithTimeout(ctx, crossEncoderTimeout)
	defer cancel()

	body, err := json.Marshal(crossEncoderRequest{TextA: textA, TextB: textB})
	if err != nil {
		return 0, fmt.Errorf("cross-encoder: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.url+"/score", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("cross-encoder: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cross-encoder: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("cross-encoder: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result crossEncoderResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("cross-encoder: decode response: %w", err)
	}

	return result.Score, nil
}
