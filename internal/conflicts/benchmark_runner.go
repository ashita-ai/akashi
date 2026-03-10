//go:build !lite

package conflicts

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/ashita-ai/akashi/internal/service/embedding"
)

// BenchmarkConfig configures a benchmark run.
type BenchmarkConfig struct {
	Provider embedding.Provider
	Logger   *slog.Logger
	Dataset  []BenchmarkPair
}

// BenchmarkResult holds the output of a benchmark run.
type BenchmarkResult struct {
	ModelName               string           `json:"model_name"`
	Dimensions              int              `json:"dimensions"`
	DatasetSize             int              `json:"dataset_size"`
	OptimalClaimTopicSim    float64          `json:"optimal_claim_topic_sim"`
	OptimalClaimDiv         float64          `json:"optimal_claim_div"`
	OptimalDecisionTopicSim float64          `json:"optimal_decision_topic_sim"`
	PrecisionAtOptimal      float64          `json:"precision_at_optimal"`
	RecallAtOptimal         float64          `json:"recall_at_optimal"`
	F1AtOptimal             float64          `json:"f1_at_optimal"`
	ThresholdSweep          []ThresholdPoint `json:"threshold_sweep"`
	SimilarityStats         SimilarityStats  `json:"similarity_stats"`
	EmbeddingLatencyMs      float64          `json:"embedding_latency_ms"`
}

// ThresholdPoint records precision/recall at a specific threshold setting.
type ThresholdPoint struct {
	ClaimTopicSimFloor    float64 `json:"claim_topic_sim_floor"`
	ClaimDivFloor         float64 `json:"claim_div_floor"`
	DecisionTopicSimFloor float64 `json:"decision_topic_sim_floor"`
	Precision             float64 `json:"precision"`
	Recall                float64 `json:"recall"`
	F1                    float64 `json:"f1"`
}

// SimilarityStats describes the observed similarity distribution for a model.
type SimilarityStats struct {
	TopicSimMean    float64 `json:"topic_sim_mean"`
	TopicSimStdDev  float64 `json:"topic_sim_std_dev"`
	TopicSimP25     float64 `json:"topic_sim_p25"`
	TopicSimP50     float64 `json:"topic_sim_p50"`
	TopicSimP75     float64 `json:"topic_sim_p75"`
	OutcomeDivMean  float64 `json:"outcome_div_mean"`
	OutcomeDivP25   float64 `json:"outcome_div_p25"`
	OutcomeDivP50   float64 `json:"outcome_div_p50"`
	OutcomeDivP75   float64 `json:"outcome_div_p75"`
	GenuineTopicSim float64 `json:"genuine_topic_sim"`   // mean topic sim for genuine conflict pairs
	GenuineOutDiv   float64 `json:"genuine_outcome_div"` // mean outcome div for genuine conflict pairs
	SampleSize      int     `json:"sample_size"`
}

// benchPairSim holds computed similarities for a single benchmark pair.
type benchPairSim struct {
	label      string
	topicSim   float64
	outcomeDiv float64
}

// RunBenchmark embeds the dataset with the given provider, sweeps threshold
// combinations, and returns the optimal thresholds with precision/recall metrics.
func RunBenchmark(ctx context.Context, cfg BenchmarkConfig) (BenchmarkResult, error) {
	if cfg.Provider == nil {
		return BenchmarkResult{}, fmt.Errorf("benchmark: embedding provider is required")
	}
	if len(cfg.Dataset) == 0 {
		cfg.Dataset = BenchmarkDataset()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	modelName := embedding.ProviderModelName(cfg.Provider)
	dims := cfg.Provider.Dimensions()

	// Collect all unique texts to embed. Each pair contributes up to 4 texts
	// (topicA, topicB, outcomeA, outcomeB), but topic and outcome may overlap.
	type embIdx struct {
		topicAIdx, topicBIdx, outcomeAIdx, outcomeBIdx int
	}

	var texts []string
	textIdx := make(map[string]int) // dedup identical strings
	addText := func(s string) int {
		if idx, ok := textIdx[s]; ok {
			return idx
		}
		idx := len(texts)
		texts = append(texts, s)
		textIdx[s] = idx
		return idx
	}

	indices := make([]embIdx, len(cfg.Dataset))
	for i, p := range cfg.Dataset {
		indices[i] = embIdx{
			topicAIdx:   addText(p.TopicA),
			topicBIdx:   addText(p.TopicB),
			outcomeAIdx: addText(p.OutcomeA),
			outcomeBIdx: addText(p.OutcomeB),
		}
	}

	cfg.Logger.Info("benchmark: embedding texts", "model", modelName, "texts", len(texts), "pairs", len(cfg.Dataset))

	start := time.Now()
	vecs, err := cfg.Provider.EmbedBatch(ctx, texts)
	if err != nil {
		return BenchmarkResult{}, fmt.Errorf("benchmark: embed batch: %w", err)
	}
	latency := time.Since(start)

	if len(vecs) != len(texts) {
		return BenchmarkResult{}, fmt.Errorf("benchmark: expected %d embeddings, got %d", len(texts), len(vecs))
	}

	// Compute pairwise similarities.
	sims := make([]benchPairSim, len(cfg.Dataset))
	for i, idx := range indices {
		topicSim := CosineSimilarity(vecs[idx.topicAIdx].Slice(), vecs[idx.topicBIdx].Slice())
		outcomeSim := CosineSimilarity(vecs[idx.outcomeAIdx].Slice(), vecs[idx.outcomeBIdx].Slice())
		outcomeDiv := math.Max(0, 1.0-outcomeSim)
		sims[i] = benchPairSim{
			label:      cfg.Dataset[i].Label,
			topicSim:   topicSim,
			outcomeDiv: outcomeDiv,
		}
	}

	// Compute similarity statistics.
	stats := computeStats(sims)

	cfg.Logger.Info("benchmark: similarity stats",
		"genuine_topic_sim", fmt.Sprintf("%.3f", stats.GenuineTopicSim),
		"genuine_outcome_div", fmt.Sprintf("%.3f", stats.GenuineOutDiv),
		"topic_sim_p50", fmt.Sprintf("%.3f", stats.TopicSimP50),
		"outcome_div_p50", fmt.Sprintf("%.3f", stats.OutcomeDivP50),
	)

	// Sweep thresholds. We sweep the decision-level topic sim floor and use the
	// full-outcome scoring path (topicSim * outcomeDiv > significance threshold)
	// since we don't have claim-level embeddings in this benchmark.
	// The claim-level thresholds only matter when claim-level scoring activates,
	// which requires the decisionTopicSimFloor to be exceeded.
	//
	// We simulate what the scorer would do: for each pair, check if
	// topicSim >= decisionTopicSimFloor AND topicSim * outcomeDiv >= sigThreshold.
	// We sweep decisionTopicSimFloor and sigThreshold to find optimal values.
	var sweep []ThresholdPoint
	var bestF1 float64
	var bestPoint ThresholdPoint

	// Sweep ranges based on observed distributions.
	for dtSim := 0.40; dtSim <= 0.85; dtSim += 0.05 {
		for sigThresh := 0.10; sigThresh <= 0.50; sigThresh += 0.05 {
			tp, fp, fn := 0, 0, 0
			for _, s := range sims {
				isGenuine := s.label == "genuine"
				sig := s.topicSim * s.outcomeDiv
				detected := s.topicSim >= dtSim && sig >= sigThresh

				switch {
				case detected && isGenuine:
					tp++
				case detected && !isGenuine:
					fp++
				case !detected && isGenuine:
					fn++
				}
			}

			var prec, recall, f1 float64
			if tp+fp > 0 {
				prec = float64(tp) / float64(tp+fp)
			}
			if tp+fn > 0 {
				recall = float64(tp) / float64(tp+fn)
			}
			if prec+recall > 0 {
				f1 = 2 * prec * recall / (prec + recall)
			}

			pt := ThresholdPoint{
				DecisionTopicSimFloor: dtSim,
				ClaimTopicSimFloor:    dtSim,
				ClaimDivFloor:         sigThresh,
				Precision:             prec,
				Recall:                recall,
				F1:                    f1,
			}
			sweep = append(sweep, pt)

			if f1 > bestF1 {
				bestF1 = f1
				bestPoint = pt
			}
		}
	}

	// If best F1 is tied, prefer higher precision.
	for _, pt := range sweep {
		if pt.F1 == bestF1 && pt.Precision > bestPoint.Precision {
			bestPoint = pt
		}
	}

	cfg.Logger.Info("benchmark: optimal thresholds",
		"decision_topic_sim", fmt.Sprintf("%.2f", bestPoint.DecisionTopicSimFloor),
		"claim_topic_sim", fmt.Sprintf("%.2f", bestPoint.ClaimTopicSimFloor),
		"claim_div", fmt.Sprintf("%.2f", bestPoint.ClaimDivFloor),
		"precision", fmt.Sprintf("%.3f", bestPoint.Precision),
		"recall", fmt.Sprintf("%.3f", bestPoint.Recall),
		"f1", fmt.Sprintf("%.3f", bestPoint.F1),
	)

	return BenchmarkResult{
		ModelName:               modelName,
		Dimensions:              dims,
		DatasetSize:             len(cfg.Dataset),
		OptimalClaimTopicSim:    bestPoint.ClaimTopicSimFloor,
		OptimalClaimDiv:         bestPoint.ClaimDivFloor,
		OptimalDecisionTopicSim: bestPoint.DecisionTopicSimFloor,
		PrecisionAtOptimal:      bestPoint.Precision,
		RecallAtOptimal:         bestPoint.Recall,
		F1AtOptimal:             bestPoint.F1,
		ThresholdSweep:          sweep,
		SimilarityStats:         stats,
		EmbeddingLatencyMs:      float64(latency.Milliseconds()),
	}, nil
}

// DeriveThresholds computes model-appropriate thresholds from observed
// similarity distributions. Uses percentile-based heuristics:
//   - claimTopicSimFloor: P75 of genuine pair topic similarities (pairs above this are "about the same thing")
//   - claimDivFloor: P25 of genuine pair outcome divergences (divergence above this indicates real disagreement)
//   - decisionTopicSimFloor: P50 of genuine pair topic similarities
func DeriveThresholds(stats SimilarityStats) (claimTopicSimFloor, claimDivFloor, decisionTopicSimFloor float64) {
	// Use the genuine pair statistics as anchors.
	// If stats don't have enough data, fall back to defaults.
	if stats.SampleSize < 10 {
		return 0.60, 0.15, 0.70
	}

	// Decision topic sim floor: below this, don't even attempt claim-level scoring.
	// Set at P50 of all topic similarities — half of all pairs should be above this.
	decisionTopicSimFloor = stats.TopicSimP50

	// Claim topic sim floor: claims must be at least this similar to be "about the same thing."
	// Set at P75 of topic similarity — only the most related pairs qualify.
	claimTopicSimFloor = stats.TopicSimP75

	// Claim div floor: minimum divergence to count as genuine disagreement.
	// Set at P25 of outcome divergence — only pairs with meaningful divergence qualify.
	claimDivFloor = stats.OutcomeDivP25

	// Sanity bounds.
	claimTopicSimFloor = clamp(claimTopicSimFloor, 0.40, 0.85)
	claimDivFloor = clamp(claimDivFloor, 0.05, 0.35)
	decisionTopicSimFloor = clamp(decisionTopicSimFloor, 0.40, 0.85)

	return claimTopicSimFloor, claimDivFloor, decisionTopicSimFloor
}

func computeStats(sims []benchPairSim) SimilarityStats {
	if len(sims) == 0 {
		return SimilarityStats{}
	}

	var topicSims, outcomeDivs []float64
	var genuineTopicSims, genuineOutDivs []float64

	for _, s := range sims {
		topicSims = append(topicSims, s.topicSim)
		outcomeDivs = append(outcomeDivs, s.outcomeDiv)
		if s.label == "genuine" {
			genuineTopicSims = append(genuineTopicSims, s.topicSim)
			genuineOutDivs = append(genuineOutDivs, s.outcomeDiv)
		}
	}

	stats := SimilarityStats{
		SampleSize: len(sims),
	}
	stats.TopicSimP25 = percentile(topicSims, 0.25)
	stats.TopicSimP50 = percentile(topicSims, 0.50)
	stats.TopicSimP75 = percentile(topicSims, 0.75)
	stats.OutcomeDivP25 = percentile(outcomeDivs, 0.25)
	stats.OutcomeDivP50 = percentile(outcomeDivs, 0.50)
	stats.OutcomeDivP75 = percentile(outcomeDivs, 0.75)

	// Mean and stddev for topic sim.
	stats.TopicSimMean = mean(topicSims)
	stats.TopicSimStdDev = stddev(topicSims, stats.TopicSimMean)
	stats.OutcomeDivMean = mean(outcomeDivs)

	if len(genuineTopicSims) > 0 {
		stats.GenuineTopicSim = mean(genuineTopicSims)
		stats.GenuineOutDiv = mean(genuineOutDivs)
	}

	return stats
}

func mean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func stddev(vs []float64, mu float64) float64 {
	if len(vs) < 2 {
		return 0
	}
	var sum float64
	for _, v := range vs {
		d := v - mu
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(vs)-1))
}

func percentile(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sorted := make([]float64, len(vs))
	copy(sorted, vs)
	sort.Float64s(sorted)
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
