//go:build !lite

package conflicts

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is a deterministic embedding provider for testing the benchmark
// runner without a real model. It maps known texts to fixed vectors.
type fakeProvider struct {
	dims    int
	vectors map[string]pgvector.Vector
}

func (f *fakeProvider) Embed(_ context.Context, text string) (pgvector.Vector, error) {
	if v, ok := f.vectors[text]; ok {
		return v, nil
	}
	return pgvector.NewVector(make([]float32, f.dims)), nil
}

func (f *fakeProvider) EmbedBatch(_ context.Context, texts []string) ([]pgvector.Vector, error) {
	vecs := make([]pgvector.Vector, len(texts))
	for i, t := range texts {
		v, _ := f.Embed(context.Background(), t)
		vecs[i] = v
	}
	return vecs, nil
}

func (f *fakeProvider) Dimensions() int {
	return f.dims
}

func (f *fakeProvider) ModelName() string {
	return "test-model"
}

func TestRunBenchmark_Basic(t *testing.T) {
	dims := 8

	// Create a minimal dataset with known embeddings.
	dataset := []BenchmarkPair{
		{
			Label:    "genuine",
			TopicA:   "cache strategy",
			TopicB:   "cache strategy",
			OutcomeA: "use redis",
			OutcomeB: "use memcached",
		},
		{
			Label:    "related_not_contradicting",
			TopicA:   "deploy",
			TopicB:   "deploy",
			OutcomeA: "deploy to prod",
			OutcomeB: "deploy to production",
		},
		{
			Label:    "unrelated_false_positive",
			TopicA:   "auth",
			TopicB:   "ci pipeline",
			OutcomeA: "use jwt",
			OutcomeB: "use github actions",
		},
	}

	// Build vectors: genuine pair has same topic, orthogonal outcomes.
	// Related pair has same topic, similar outcomes.
	// Unrelated pair has orthogonal topics.
	vectors := map[string]pgvector.Vector{}
	// Genuine: same topic, different outcomes
	vectors["cache strategy"] = pgvector.NewVector([]float32{1, 0, 0, 0, 0, 0, 0, 0})
	vectors["use redis"] = pgvector.NewVector([]float32{0, 1, 0, 0, 0, 0, 0, 0})
	vectors["use memcached"] = pgvector.NewVector([]float32{0, 0, 1, 0, 0, 0, 0, 0})
	// Related: same topic, similar outcomes
	vectors["deploy"] = pgvector.NewVector([]float32{0, 0, 0, 1, 0, 0, 0, 0})
	vectors["deploy to prod"] = pgvector.NewVector([]float32{0, 0, 0, 0, 0.95, 0.31, 0, 0})
	vectors["deploy to production"] = pgvector.NewVector([]float32{0, 0, 0, 0, 1, 0, 0, 0})
	// Unrelated: different topics
	vectors["auth"] = pgvector.NewVector([]float32{0, 0, 0, 0, 0, 0, 1, 0})
	vectors["ci pipeline"] = pgvector.NewVector([]float32{0, 0, 0, 0, 0, 0, 0, 1})
	vectors["use jwt"] = pgvector.NewVector([]float32{0, 0, 0, 0, 0, 1, 0, 0})
	vectors["use github actions"] = pgvector.NewVector([]float32{0, 0, 0, 0, 0, 0, 0, 1})

	provider := &fakeProvider{dims: dims, vectors: vectors}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	result, err := RunBenchmark(context.Background(), BenchmarkConfig{
		Provider: provider,
		Logger:   logger,
		Dataset:  dataset,
	})
	require.NoError(t, err)

	assert.Equal(t, "test-model", result.ModelName)
	assert.Equal(t, dims, result.Dimensions)
	assert.Equal(t, 3, result.DatasetSize)
	assert.Greater(t, len(result.ThresholdSweep), 0, "should have threshold sweep points")
	assert.Greater(t, result.SimilarityStats.SampleSize, 0)
}

func TestDeriveThresholds_Defaults(t *testing.T) {
	// Insufficient data — should return defaults.
	claimSim, claimDiv, decSim := DeriveThresholds(SimilarityStats{SampleSize: 5})
	assert.Equal(t, 0.60, claimSim)
	assert.Equal(t, 0.15, claimDiv)
	assert.Equal(t, 0.70, decSim)
}

func TestDeriveThresholds_FromStats(t *testing.T) {
	stats := SimilarityStats{
		SampleSize: 30,
	}
	stats.TopicSimP50 = 0.55
	stats.TopicSimP75 = 0.72
	stats.OutcomeDivP25 = 0.10

	claimSim, claimDiv, decSim := DeriveThresholds(stats)
	assert.Equal(t, 0.72, claimSim, "claimTopicSimFloor should be P75 of topic sim")
	assert.Equal(t, 0.10, claimDiv, "claimDivFloor should be P25 of outcome div")
	assert.Equal(t, 0.55, decSim, "decisionTopicSimFloor should be P50 of topic sim")
}

func TestComputeStats(t *testing.T) {
	sims := []benchPairSim{
		{label: "genuine", topicSim: 0.9, outcomeDiv: 0.8},
		{label: "genuine", topicSim: 0.85, outcomeDiv: 0.7},
		{label: "related_not_contradicting", topicSim: 0.8, outcomeDiv: 0.05},
		{label: "unrelated_false_positive", topicSim: 0.1, outcomeDiv: 0.9},
	}

	stats := computeStats(sims)
	assert.Equal(t, 4, stats.SampleSize)
	assert.InDelta(t, 0.875, stats.GenuineTopicSim, 0.001, "genuine topic sim should be mean of 0.9 and 0.85")
	assert.InDelta(t, 0.75, stats.GenuineOutDiv, 0.001, "genuine outcome div should be mean of 0.8 and 0.7")
}

func TestBenchmarkDataset_Labels(t *testing.T) {
	dataset := BenchmarkDataset()
	assert.Equal(t, 30, len(dataset), "should have 30 pairs (10 per category)")

	counts := map[string]int{}
	for _, p := range dataset {
		counts[p.Label]++
		assert.NotEmpty(t, p.TopicA)
		assert.NotEmpty(t, p.TopicB)
		assert.NotEmpty(t, p.OutcomeA)
		assert.NotEmpty(t, p.OutcomeB)
	}
	assert.Equal(t, 10, counts["genuine"])
	assert.Equal(t, 10, counts["related_not_contradicting"])
	assert.Equal(t, 10, counts["unrelated_false_positive"])
}
