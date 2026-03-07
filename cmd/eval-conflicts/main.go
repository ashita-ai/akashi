// eval-conflicts runs the conflict validator evaluation dataset against a
// running akashi instance and reports precision/recall metrics.
//
// Usage:
//
//	AKASHI_URL=http://localhost:8081 AKASHI_API_KEY=ak_... go run ./cmd/eval-conflicts
//
// Environment variables:
//
//	AKASHI_URL      Base URL of the akashi server (default: http://localhost:8081)
//	AKASHI_API_KEY  API key for admin authentication (required)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/ashita-ai/akashi/internal/conflicts"
)

func main() {
	os.Exit(run())
}

func run() int {
	baseURL := os.Getenv("AKASHI_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	apiKey := os.Getenv("AKASHI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "AKASHI_API_KEY is required")
		return 1
	}

	// Authenticate to get a JWT.
	token, err := authenticate(baseURL, apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "targeting %s\n", baseURL)
	fmt.Fprintf(os.Stderr, "running eval dataset (%d pairs)...\n", len(conflicts.DefaultEvalDataset()))

	// Call the eval endpoint.
	metrics, results, err := runEval(baseURL, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval failed: %v\n", err)
		return 1
	}

	// Print results.
	fmt.Print(conflicts.FormatMetrics(metrics, results))

	// Exit non-zero if precision or recall is below threshold.
	if metrics.ConflictPrec < 0.80 {
		fmt.Fprintf(os.Stderr, "\nFAIL: conflict precision %.1f%% < 80%%\n", metrics.ConflictPrec*100)
		return 1
	}
	if metrics.ConflictRecall < 0.80 {
		fmt.Fprintf(os.Stderr, "\nFAIL: conflict recall %.1f%% < 80%%\n", metrics.ConflictRecall*100)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nPASS: precision=%.1f%% recall=%.1f%%\n", metrics.ConflictPrec*100, metrics.ConflictRecall*100)
	return 0
}

func authenticate(baseURL, apiKey string) (string, error) {
	authURL, err := url.JoinPath(baseURL, "/auth/token")
	if err != nil {
		return "", fmt.Errorf("build auth URL: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"api_key": apiKey})
	resp, err := http.Post(authURL, "application/json", bytes.NewReader(body)) //nolint:gosec // URL is operator-provided via AKASHI_URL env var
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	return result.Token, nil
}

type evalResponse struct {
	Metrics conflicts.EvalMetrics  `json:"metrics"`
	Results []conflicts.EvalResult `json:"results"`
}

func runEval(baseURL, token string) (conflicts.EvalMetrics, []conflicts.EvalResult, error) {
	evalURL, err := url.JoinPath(baseURL, "/v1/admin/conflicts/eval")
	if err != nil {
		return conflicts.EvalMetrics{}, nil, fmt.Errorf("build eval URL: %w", err)
	}
	req, err := http.NewRequest("POST", evalURL, bytes.NewReader([]byte("{}"))) //nolint:gosec // URL is operator-provided via AKASHI_URL env var
	if err != nil {
		return conflicts.EvalMetrics{}, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Minute} // LLM calls can be slow.
	resp, err := client.Do(req)                       //nolint:gosec // req uses operator-provided URL
	if err != nil {
		return conflicts.EvalMetrics{}, nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return conflicts.EvalMetrics{}, nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var result evalResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return conflicts.EvalMetrics{}, nil, fmt.Errorf("decode: %w", err)
	}
	return result.Metrics, result.Results, nil
}
