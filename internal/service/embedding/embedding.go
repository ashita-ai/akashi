// Package embedding provides vector embedding generation for semantic search.
//
// Defines an EmbeddingProvider interface and an OpenAI implementation.
// The interface allows swapping embedding providers without changing consumers.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/pgvector/pgvector-go"
)

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
func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	dims := 1536
	return &OpenAIProvider{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{},
		dimensions: dims,
	}
}

// Dimensions returns the embedding vector size.
func (p *OpenAIProvider) Dimensions() int {
	return p.dimensions
}

type openAIRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
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

	reqBody, err := json.Marshal(openAIRequest{Input: texts, Model: p.model})
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
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embedding: read response: %w", err)
	}

	var result openAIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("embedding: unmarshal response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("embedding: openai error: %s: %s", result.Error.Type, result.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding: unexpected status %d: %s", resp.StatusCode, string(body))
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

// Embed returns a zero vector.
func (p *NoopProvider) Embed(_ context.Context, _ string) (pgvector.Vector, error) {
	return pgvector.NewVector(make([]float32, p.dims)), nil
}

// EmbedBatch returns zero vectors.
func (p *NoopProvider) EmbedBatch(_ context.Context, texts []string) ([]pgvector.Vector, error) {
	vecs := make([]pgvector.Vector, len(texts))
	for i := range vecs {
		vecs[i] = pgvector.NewVector(make([]float32, p.dims))
	}
	return vecs, nil
}
