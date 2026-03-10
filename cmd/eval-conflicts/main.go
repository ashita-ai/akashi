// eval-conflicts runs conflict detection evaluation against a running akashi instance.
//
// Two modes:
//
//	--mode=validator  (default) Runs the hardcoded eval dataset through the LLM validator.
//	--mode=scorer     Computes scorer precision from ground truth labels.
//
// Usage:
//
//	AKASHI_AGENT_ID=admin AKASHI_API_KEY=ak_... go run ./cmd/eval-conflicts
//	AKASHI_AGENT_ID=admin AKASHI_API_KEY=ak_... go run ./cmd/eval-conflicts --mode=scorer
//	AKASHI_URL=http://localhost:8081 AKASHI_AGENT_ID=admin AKASHI_API_KEY=ak_... go run ./cmd/eval-conflicts
//
// Environment variables:
//
//	AKASHI_URL       Base URL of the akashi server (default: http://localhost:8081)
//	AKASHI_AGENT_ID  Agent ID for authentication (required)
//	AKASHI_API_KEY   API key for admin authentication (required)
//
// Use --save to persist results as JSON files in ./eval-results/.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/service/embedding"
)

func main() {
	os.Exit(run())
}

func run() int {
	mode := flag.String("mode", "validator", "evaluation mode: validator, scorer, or benchmark")
	save := flag.Bool("save", false, "save results to ./eval-results/{timestamp}.json")
	flag.Parse()

	// Benchmark mode runs locally — no server or auth required.
	if *mode == "benchmark" {
		return runBenchmarkMode(*save)
	}

	baseURL := os.Getenv("AKASHI_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	agentID := os.Getenv("AKASHI_AGENT_ID")
	if agentID == "" {
		fmt.Fprintln(os.Stderr, "AKASHI_AGENT_ID is required")
		return 1
	}
	apiKey := os.Getenv("AKASHI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "AKASHI_API_KEY is required")
		return 1
	}

	token, err := authenticate(baseURL, agentID, apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "targeting %s (mode=%s)\n", baseURL, *mode)

	switch *mode {
	case "validator":
		return runValidatorEval(baseURL, token, *save)
	case "scorer":
		return runScorerEval(baseURL, token, *save)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s (use 'validator', 'scorer', or 'benchmark')\n", *mode)
		return 1
	}
}

func runValidatorEval(baseURL, token string, save bool) int {
	fmt.Fprintf(os.Stderr, "running validator eval dataset (%d pairs)...\n", len(conflicts.DefaultEvalDataset()))

	metrics, results, err := callValidatorEval(baseURL, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval failed: %v\n", err)
		return 1
	}

	fmt.Print(conflicts.FormatMetrics(metrics, results))

	if save {
		if err := saveResults("validator", map[string]any{
			"metrics": metrics,
			"results": results,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save results: %v\n", err)
		}
	}

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

type scorerEvalResult struct {
	Precision      float64 `json:"precision"`
	TruePositives  int     `json:"true_positives"`
	FalsePositives int     `json:"false_positives"`
	TotalLabeled   int     `json:"total_labeled"`
	Message        string  `json:"message,omitempty"`
}

func runScorerEval(baseURL, token string, save bool) int {
	fmt.Fprintln(os.Stderr, "running scorer precision eval from ground truth labels...")

	result, err := callScorerEval(baseURL, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval failed: %v\n", err)
		return 1
	}

	if result.TotalLabeled == 0 {
		fmt.Fprintln(os.Stderr, "no labeled conflicts found — label some first:")
		fmt.Fprintln(os.Stderr, "  curl -X PUT http://localhost:8081/v1/admin/conflicts/{id}/label \\")
		fmt.Fprintln(os.Stderr, "    -H 'Authorization: Bearer ...' \\")
		fmt.Fprintln(os.Stderr, "    -d '{\"label\": \"genuine\"}'")
		return 0
	}

	fmt.Printf("Scorer Precision: %.1f%% (%d TP, %d FP, %d labeled)\n",
		result.Precision*100, result.TruePositives, result.FalsePositives, result.TotalLabeled)

	if save {
		if err := saveResults("scorer", result); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save results: %v\n", err)
		}
	}

	return 0
}

func saveResults(mode string, data any) error {
	dir := "eval-results"
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.json", mode, time.Now().Format("2006-01-02T15-04-05"))
	path := filepath.Join(dir, filename)

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Fprintf(os.Stderr, "results saved to %s\n", path)
	return nil
}

func authenticate(baseURL, agentID, apiKey string) (string, error) {
	authURL, err := url.JoinPath(baseURL, "/auth/token")
	if err != nil {
		return "", fmt.Errorf("build auth URL: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"agent_id": agentID, "api_key": apiKey})
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
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if result.Data.Token == "" {
		return "", fmt.Errorf("empty token in response")
	}
	return result.Data.Token, nil
}

type validatorEvalResponse struct {
	Metrics conflicts.EvalMetrics  `json:"metrics"`
	Results []conflicts.EvalResult `json:"results"`
}

func callValidatorEval(baseURL, token string) (conflicts.EvalMetrics, []conflicts.EvalResult, error) {
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

	var envelope struct {
		Data validatorEvalResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return conflicts.EvalMetrics{}, nil, fmt.Errorf("decode: %w", err)
	}
	return envelope.Data.Metrics, envelope.Data.Results, nil
}

func runBenchmarkMode(save bool) int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	provider, err := newBenchmarkProvider(logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create embedding provider: %v\n", err)
		return 1
	}

	modelName := embedding.ProviderModelName(provider)
	fmt.Fprintf(os.Stderr, "benchmarking model: %s (%d dimensions)\n", modelName, provider.Dimensions())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := conflicts.RunBenchmark(ctx, conflicts.BenchmarkConfig{
		Provider: provider,
		Logger:   logger,
		Dataset:  conflicts.BenchmarkDataset(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark failed: %v\n", err)
		return 1
	}

	// Print summary.
	fmt.Printf("\n=== Embedding Model Benchmark: %s ===\n", result.ModelName)
	fmt.Printf("Dimensions: %d\n", result.Dimensions)
	fmt.Printf("Dataset:    %d pairs\n", result.DatasetSize)
	fmt.Printf("Latency:    %.0f ms (total batch embed)\n\n", result.EmbeddingLatencyMs)

	fmt.Printf("Similarity Distribution:\n")
	fmt.Printf("  Topic Sim:    mean=%.3f  stddev=%.3f  P25=%.3f  P50=%.3f  P75=%.3f\n",
		result.SimilarityStats.TopicSimMean, result.SimilarityStats.TopicSimStdDev,
		result.SimilarityStats.TopicSimP25, result.SimilarityStats.TopicSimP50, result.SimilarityStats.TopicSimP75)
	fmt.Printf("  Outcome Div:  mean=%.3f  P25=%.3f  P50=%.3f  P75=%.3f\n",
		result.SimilarityStats.OutcomeDivMean,
		result.SimilarityStats.OutcomeDivP25, result.SimilarityStats.OutcomeDivP50, result.SimilarityStats.OutcomeDivP75)
	fmt.Printf("  Genuine:      topic_sim=%.3f  outcome_div=%.3f\n\n",
		result.SimilarityStats.GenuineTopicSim, result.SimilarityStats.GenuineOutDiv)

	fmt.Printf("Optimal Thresholds:\n")
	fmt.Printf("  ClaimTopicSimFloor:    %.2f\n", result.OptimalClaimTopicSim)
	fmt.Printf("  ClaimDivFloor:         %.2f\n", result.OptimalClaimDiv)
	fmt.Printf("  DecisionTopicSimFloor: %.2f\n\n", result.OptimalDecisionTopicSim)

	fmt.Printf("Performance at Optimal:\n")
	fmt.Printf("  Precision: %.1f%%\n", result.PrecisionAtOptimal*100)
	fmt.Printf("  Recall:    %.1f%%\n", result.RecallAtOptimal*100)
	fmt.Printf("  F1:        %.1f%%\n\n", result.F1AtOptimal*100)

	// Derive thresholds from stats.
	claimSim, claimDiv, decSim := conflicts.DeriveThresholds(result.SimilarityStats)
	fmt.Printf("Auto-Derived Thresholds (from stats):\n")
	fmt.Printf("  ClaimTopicSimFloor:    %.2f\n", claimSim)
	fmt.Printf("  ClaimDivFloor:         %.2f\n", claimDiv)
	fmt.Printf("  DecisionTopicSimFloor: %.2f\n\n", decSim)

	// Suggested config snippet.
	fmt.Printf("Suggested config/profile entry:\n")
	fmt.Printf("  \"%s\": {\n", modelName)
	fmt.Printf("    claimTopicSimFloor:    %.2f,\n", result.OptimalClaimTopicSim)
	fmt.Printf("    claimDivFloor:         %.2f,\n", result.OptimalClaimDiv)
	fmt.Printf("    decisionTopicSimFloor: %.2f,\n", result.OptimalDecisionTopicSim)
	fmt.Printf("  }\n")

	if save {
		if err := saveResults("benchmark", result); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save results: %v\n", err)
		}
	}
	return 0
}

// newBenchmarkProvider creates an embedding provider from environment variables.
// Supports OLLAMA_URL/OLLAMA_MODEL for Ollama and OPENAI_API_KEY/AKASHI_EMBEDDING_MODEL for OpenAI.
func newBenchmarkProvider(logger *slog.Logger) (embedding.Provider, error) {
	providerType := os.Getenv("AKASHI_EMBEDDING_PROVIDER")
	if providerType == "" {
		providerType = "auto"
	}

	dims := 1024
	if d := os.Getenv("AKASHI_EMBEDDING_DIMENSIONS"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil {
			return nil, fmt.Errorf("invalid AKASHI_EMBEDDING_DIMENSIONS: %w", err)
		}
		dims = n
	}

	switch providerType {
	case "openai":
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY is required for openai provider")
		}
		model := os.Getenv("AKASHI_EMBEDDING_MODEL")
		if model == "" {
			model = "text-embedding-3-small"
		}
		logger.Info("benchmark: using OpenAI provider", "model", model, "dims", dims)
		return embedding.NewOpenAIProvider(apiKey, model, dims)
	case "ollama", "auto":
		ollamaURL := os.Getenv("OLLAMA_URL")
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}
		model := os.Getenv("OLLAMA_MODEL")
		if model == "" {
			model = "mxbai-embed-large"
		}
		logger.Info("benchmark: using Ollama provider", "url", ollamaURL, "model", model, "dims", dims)
		return embedding.NewOllamaProvider(ollamaURL, model, dims), nil
	default:
		return nil, fmt.Errorf("unsupported embedding provider: %s", providerType)
	}
}

func callScorerEval(baseURL, token string) (scorerEvalResult, error) {
	evalURL, err := url.JoinPath(baseURL, "/v1/admin/scorer-eval")
	if err != nil {
		return scorerEvalResult{}, fmt.Errorf("build eval URL: %w", err)
	}
	req, err := http.NewRequest("POST", evalURL, bytes.NewReader([]byte("{}"))) //nolint:gosec // URL is operator-provided via AKASHI_URL env var
	if err != nil {
		return scorerEvalResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // req uses operator-provided URL
	if err != nil {
		return scorerEvalResult{}, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return scorerEvalResult{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var envelope struct {
		Data scorerEvalResult `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return scorerEvalResult{}, fmt.Errorf("decode: %w", err)
	}
	return envelope.Data, nil
}
