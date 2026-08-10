// eval-conflicts runs conflict detection evaluation against a running akashi instance.
//
// Modes:
//
//	--mode=validator  (default) Runs the hardcoded eval dataset through the LLM validator.
//	--mode=scorer     Computes scorer precision from ground truth labels.
//	--mode=benchmark  Runs the local scoring benchmark; no server or auth needed.
//	--mode=gold       Runs blind gold labels through the LLM validator. Talks to the
//	                  database directly (AKASHI_DB_DSN + OPENAI_API_KEY), not the server.
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
//	AKASHI_DB_DSN    Postgres DSN (gold mode only)
//	AKASHI_ORG_ID    Org whose gold labels to evaluate (gold mode, default: all-zero UUID)
//	OPENAI_API_KEY   OpenAI key for the judge under test (gold mode only)
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/service/embedding"
)

func main() {
	os.Exit(run())
}

func run() int {
	mode := flag.String("mode", "validator", "evaluation mode: validator, scorer, benchmark, or gold")
	goldLimit := flag.Int("gold-limit", 200, "gold mode: max labeled pairs to evaluate (0 = all)")
	goldConc := flag.Int("gold-conc", 8, "gold mode: concurrent validator calls")
	goldModel := flag.String("gold-model", "gpt-4o-mini", "gold mode: OpenAI model to evaluate")
	goldTimeout := flag.Duration("gold-timeout", 15*time.Second, "gold mode: per-call deadline (raise for reasoning models)")
	goldClasses := flag.String("gold-classes", "", "gold mode: comma-separated expected relationships to evaluate (default: stratified across all). Use to measure the false-positive rate on the majority class, which is what bounds precision at a low base rate.")
	save := flag.Bool("save", false, "save results to ./eval-results/{timestamp}.json")
	flag.Parse()

	// Benchmark mode runs locally — no server or auth required.
	if *mode == "benchmark" {
		return runBenchmarkMode(*save)
	}

	// Gold mode talks to the database directly, not the server.
	if *mode == "gold" {
		return runGoldMode(*goldLimit, *goldConc, *goldModel, *goldTimeout, *goldClasses, *save)
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
		fmt.Fprintf(os.Stderr, "unknown mode: %s (use 'validator', 'scorer', 'benchmark', or 'gold')\n", *mode)
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

// runGoldMode evaluates the live validator against blind gold labels.
//
// Unlike --mode=validator, which runs the hand-written DefaultEvalDataset, this
// mode reads real scored pairs and their blind classifications from
// conflict_gold_labels. That distinction is the point: the hand-written dataset
// scored a perfect 1.000/1.000 while the shipped detector was running at 3.4%
// precision, because a dataset written from the same intuitions as the prompt
// cannot falsify that prompt.
//
// Requires AKASHI_DB_DSN (direct database access) and OPENAI_API_KEY.
func runGoldMode(limit, conc int, model string, timeout time.Duration, classes string, save bool) int {
	dsn := os.Getenv("AKASHI_DB_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "gold mode requires AKASHI_DB_DSN")
		return 1
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "gold mode requires OPENAI_API_KEY")
		return 1
	}
	orgID := os.Getenv("AKASHI_ORG_ID")
	if orgID == "" {
		orgID = "00000000-0000-0000-0000-000000000000"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		return 1
	}
	defer pool.Close()

	pairs, err := conflicts.LoadGoldPairs(ctx, pool, orgID, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load gold:", err)
		return 1
	}
	if len(pairs) == 0 {
		fmt.Fprintln(os.Stderr, "no gold labels found for org", orgID)
		return 1
	}
	fmt.Println(conflicts.GoldSummary(pairs))
	if classes != "" {
		want := map[string]bool{}
		for _, c := range strings.Split(classes, ",") {
			want[strings.TrimSpace(c)] = true
		}
		filtered := pairs[:0:0]
		for _, p := range pairs {
			if want[p.ExpectedRelationship] {
				filtered = append(filtered, p)
			}
		}
		pairs = spread(filtered, limit)
	} else {
		pairs = stratifyGold(pairs, limit)
	}
	fmt.Println("sampled \u2192", conflicts.GoldSummary(pairs))

	v := conflicts.NewOpenAIValidator(apiKey, model, conflicts.WithRequestTimeout(timeout))
	results := make([]conflicts.EvalResult, len(pairs))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, p := range pairs {
		wg.Add(1)
		go func(i int, p conflicts.EvalPair) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			er := conflicts.EvalResult{Label: p.Label, ExpectedRelationship: p.ExpectedRelationship}
			res, err := v.Validate(ctx, p.Input)
			if err != nil {
				er.Error = err.Error()
			} else {
				er.ActualRelationship = res.Relationship
				er.Correct = conflicts.RelationshipEquivalent(p.ExpectedRelationship, res.Relationship)
				er.ConflictExpected = p.ExpectedRelationship == "contradiction"
				er.ConflictActual = res.Relationship == "contradiction"
				er.Explanation = res.Explanation
			}
			results[i] = er
		}(i, p)
	}
	wg.Wait()

	reportGold(results, model)
	if save {
		if err := saveResults("gold", results); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			return 1
		}
	}
	return 0
}

// stratifyGold oversamples the rare, decision-relevant classes so a bounded run
// still measures contradiction recall. reportGold re-weights back to true class
// sizes before quoting precision.
func stratifyGold(pairs []conflicts.EvalPair, limit int) []conflicts.EvalPair {
	if limit <= 0 || len(pairs) <= limit {
		return pairs
	}
	byRel := map[string][]conflicts.EvalPair{}
	for _, p := range pairs {
		byRel[p.ExpectedRelationship] = append(byRel[p.ExpectedRelationship], p)
	}
	kept := append([]conflicts.EvalPair{}, byRel["contradiction"]...)
	kept = append(kept, spread(byRel["supersession"], limit/4)...)
	kept = append(kept, spread(byRel["unrelated"], limit/20)...)
	if rest := limit - len(kept); rest > 0 {
		kept = append(kept, spread(byRel["complementary"], rest)...)
	}
	return kept
}

// spread samples n items evenly across the slice rather than taking the first n,
// which would draw every pair from one time window.
func spread(ps []conflicts.EvalPair, n int) []conflicts.EvalPair {
	if n <= 0 {
		return nil
	}
	if len(ps) <= n {
		return ps
	}
	step := len(ps) / n
	out := make([]conflicts.EvalPair, 0, n)
	for i := 0; i < len(ps) && len(out) < n; i += step {
		out = append(out, ps[i])
	}
	return out
}

// goldCorpusSizes are the true class counts of the labeled corpus, used to
// project sample results back onto the queue a user would actually see.
var goldCorpusSizes = map[string]float64{
	"contradiction": 93, "supersession": 627, "complementary": 2017, "unrelated": 35,
}

func reportGold(results []conflicts.EvalResult, model string) {
	m := conflicts.ComputeMetrics(results)
	fmt.Printf("\n=== gold-set eval (model=%s, n=%d, errors=%d) ===\n", model, m.TotalPairs, m.Errors)
	fmt.Printf("relationship accuracy  : %.1f%%\n", m.RelationshipAcc*100)
	fmt.Printf("contradiction precision: %.1f%%  recall: %.1f%%  F1: %.3f  (stratified sample)\n",
		m.ConflictPrec*100, m.ConflictRecall*100, m.ConflictF1)

	conf := map[string]map[string]int{}
	for _, r := range results {
		if r.Error != "" {
			continue
		}
		if conf[r.ExpectedRelationship] == nil {
			conf[r.ExpectedRelationship] = map[string]int{}
		}
		conf[r.ExpectedRelationship][r.ActualRelationship]++
	}

	// Sample precision flatters the judge because contradictions are
	// oversampled. Re-weight per-class flag rates by true corpus sizes.
	var flagged, tp float64
	for expected, acts := range conf {
		total := 0
		for _, n := range acts {
			total += n
		}
		if total == 0 {
			continue
		}
		n := goldCorpusSizes[expected] * float64(acts["contradiction"]) / float64(total)
		flagged += n
		if expected == "contradiction" {
			tp = n
		}
	}
	if flagged > 0 {
		base := goldCorpusSizes["contradiction"]
		var totalCorpus float64
		for _, n := range goldCorpusSizes {
			totalCorpus += n
		}
		baseRate := base / totalCorpus
		fmt.Printf("corpus-projected queue : %.0f pairs, precision %.1f%% (base rate %.1f%%, lift %.1fx)\n",
			flagged, tp/flagged*100, baseRate*100, (tp/flagged)/baseRate)
	}

	fmt.Println("\ngold (row) \u2192 validator (col):")
	rels := make([]string, 0, len(conf))
	for k := range conf {
		rels = append(rels, k)
	}
	sort.Strings(rels)
	for _, e := range rels {
		acts := make([]string, 0, len(conf[e]))
		total := 0
		for k, n := range conf[e] {
			acts = append(acts, k)
			total += n
		}
		sort.Strings(acts)
		fmt.Printf("  %-14s ", e)
		for _, a := range acts {
			fmt.Printf("%s=%d (%.0f%%)  ", a, conf[e][a], float64(conf[e][a])/float64(total)*100)
		}
		fmt.Println()
	}
}
