// Package embedding provides vector embedding generation for semantic search.
//
// Defines an EmbeddingProvider interface and an OpenAI implementation.
// The interface allows swapping embedding providers without changing consumers.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pgvector/pgvector-go"
)

// ErrNoProvider is returned by NoopProvider to signal that no real embedding
// provider is configured. Callers should treat this as "no embedding available"
// rather than a transient failure.
var ErrNoProvider = errors.New("embedding: no provider configured (noop)")

// maxResponseBody is the maximum size of an OpenAI embedding response we'll read (10 MB).
const maxResponseBody = 10 * 1024 * 1024

// Provider generates vector embeddings from text.
type Provider interface {
	// Embed generates a single embedding vector from text.
	Embed(ctx context.Context, text string) (pgvector.Vector, error)

	// EmbedBatch generates embeddings for multiple texts.
	EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error)

	// Dimensions returns the embedding vector dimensionality.
	Dimensions() int
}

// OpenAIProvider generates embeddings using the OpenAI API.
type OpenAIProvider struct {
	apiKey     string
	model      string
	httpClient *http.Client
	dimensions int
}

// NewOpenAIProvider creates a new OpenAI embedding provider.
// Dimensions should match the model's output size (e.g., 1536 for text-embedding-3-small,
// or a smaller value if using the dimensions parameter in the API request).
// Returns an error if apiKey is empty.
func NewOpenAIProvider(apiKey, model string, dimensions int) (*OpenAIProvider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("embedding: OpenAI API key is required")
	}
	if dimensions <= 0 {
		dimensions = 1536 // Default for text-embedding-3-small
	}
	return &OpenAIProvider{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		dimensions: dimensions,
	}, nil
}

// Dimensions returns the embedding vector size.
func (p *OpenAIProvider) Dimensions() int {
	return p.dimensions
}

type openAIRequest struct {
	Input      []string `json:"input"`
	Model      string   `json:"model"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type openAIResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Embed generates a single embedding.
func (p *OpenAIProvider) Embed(ctx context.Context, text string) (pgvector.Vector, error) {
	vecs, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return pgvector.Vector{}, err
	}
	return vecs[0], nil
}

// EmbedBatch generates embeddings for multiple texts in a single API call.
func (p *OpenAIProvider) EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody, err := json.Marshal(openAIRequest{Input: texts, Model: p.model, Dimensions: p.dimensions})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embedding: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("embedding: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to parse a structured error from the body; fall back to raw body.
		var errResp openAIResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != nil {
			return nil, fmt.Errorf("embedding: openai error (HTTP %d): %s: %s", resp.StatusCode, errResp.Error.Type, errResp.Error.Message)
		}
		return nil, fmt.Errorf("embedding: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result openAIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("embedding: unmarshal response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("embedding: openai error: %s: %s", result.Error.Type, result.Error.Message)
	}

	if len(result.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: expected %d embeddings but got %d", len(texts), len(result.Data))
	}

	// Ensure results are in input order.
	vecs := make([]pgvector.Vector, len(texts))
	for _, d := range result.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embedding: invalid index %d in response", d.Index)
		}
		vecs[d.Index] = pgvector.NewVector(d.Embedding)
	}

	return vecs, nil
}

// NoopProvider returns zero vectors. Used when no API key is configured.
type NoopProvider struct {
	dims int
}

// NewNoopProvider creates a provider that returns zero vectors.
func NewNoopProvider(dims int) *NoopProvider {
	return &NoopProvider{dims: dims}
}

// Dimensions returns the embedding vector size.
func (p *NoopProvider) Dimensions() int {
	return p.dims
}

// Embed returns ErrNoProvider. Callers skip embedding storage on error,
// avoiding ~4KB of zero-vector bloat per decision in Postgres.
func (p *NoopProvider) Embed(_ context.Context, _ string) (pgvector.Vector, error) {
	return pgvector.Vector{}, ErrNoProvider
}

// EmbedBatch returns ErrNoProvider.
func (p *NoopProvider) EmbedBatch(_ context.Context, _ []string) ([]pgvector.Vector, error) {
	return nil, ErrNoProvider
}
