package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"
)

// OllamaProvider generates embeddings using a local Ollama server.
// This is the recommended provider for production: embeddings stay on-premises,
// no external API costs, and data never leaves the customer's network.
type OllamaProvider struct {
	baseURL    string
	model      string
	httpClient *http.Client
	dimensions int
}

// NewOllamaProvider creates a provider that calls Ollama's embedding API.
// Model should be an embedding model like "mxbai-embed-large" or "nomic-embed-text".
// Dimensions must match the model's native output size (e.g., 1024 for mxbai-embed-large).
func NewOllamaProvider(baseURL, model string, dimensions int) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaProvider{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		dimensions: dimensions,
	}
}

// Dimensions returns the model's native vector size.
func (p *OllamaProvider) Dimensions() int {
	return p.dimensions
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed generates a single embedding vector from text.
func (p *OllamaProvider) Embed(ctx context.Context, text string) (pgvector.Vector, error) {
	reqBody, err := json.Marshal(ollamaEmbedRequest{
		Model:  p.model,
		Prompt: text,
	})
	if err != nil {
		return pgvector.Vector{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return pgvector.Vector{}, fmt.Errorf("ollama: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return pgvector.Vector{}, fmt.Errorf("ollama: send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return pgvector.Vector{}, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(body))
	}

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return pgvector.Vector{}, fmt.Errorf("ollama: decode response: %w", err)
	}

	if len(result.Embedding) == 0 {
		return pgvector.Vector{}, fmt.Errorf("ollama: empty embedding returned")
	}

	return pgvector.NewVector(result.Embedding), nil
}

// ollamaMaxConcurrency is the maximum number of parallel requests to Ollama.
// Kept low to avoid overwhelming a single local GPU.
const ollamaMaxConcurrency = 4

// EmbedBatch generates embeddings for multiple texts.
// Ollama doesn't have a native batch API, so we call concurrently with
// a bounded worker pool to reduce wall-clock time.
func (p *OllamaProvider) EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Single text — no concurrency overhead.
	if len(texts) == 1 {
		vec, err := p.Embed(ctx, texts[0])
		if err != nil {
			return nil, err
		}
		return []pgvector.Vector{vec}, nil
	}

	vecs := make([]pgvector.Vector, len(texts))
	errs := make([]error, len(texts))
	sem := make(chan struct{}, ollamaMaxConcurrency)

	var wg sync.WaitGroup
	for i, text := range texts {
		wg.Add(1)
		go func(idx int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			vec, err := p.Embed(ctx, t)
			if err != nil {
				errs[idx] = fmt.Errorf("ollama: batch item %d: %w", idx, err)
				return
			}
			vecs[idx] = vec
		}(i, text)
	}
	wg.Wait()

	// Return the first error encountered.
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return vecs, nil
}
