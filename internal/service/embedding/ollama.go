package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"
)

// OllamaProvider generates embeddings using a local Ollama server.
// This is the recommended provider for production: embeddings stay on-premises,
// no external API costs, and data never leaves the customer's network.
type OllamaProvider struct {
	baseURL       string
	model         string
	httpClient    *http.Client
	dimensions    int
	maxInputChars int // Maximum input length in characters. Text beyond this is truncated.
}

// defaultMaxInputChars is a safe default for mxbai-embed-large (512 tokens).
// At ~4 chars/token for English prose, 2000 chars ≈ 500 tokens. Code-heavy
// content tokenizes at ~2-3 chars/token, so this is slightly aggressive for
// that case. The /api/embed endpoint truncates as a safety net if we overshoot.
const defaultMaxInputChars = 2000

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
		dimensions:    dimensions,
		maxInputChars: defaultMaxInputChars,
	}
}

// Dimensions returns the model's native vector size.
func (p *OllamaProvider) Dimensions() int {
	return p.dimensions
}

// ollamaEmbedRequest is the request body for POST /api/embed.
// The input field accepts a single string or an array of strings.
type ollamaEmbedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string or []string
}

// ollamaEmbedResponse is the response body from POST /api/embed.
// Embeddings is always an array of arrays, even for single inputs.
type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed generates a single embedding vector from text.
// Text exceeding maxInputChars is truncated at a word boundary to stay within
// the model's context window. The /api/embed endpoint provides a second safety
// net by truncating at the token level if the char-based estimate overshoots.
func (p *OllamaProvider) Embed(ctx context.Context, text string) (pgvector.Vector, error) {
	text = truncateText(text, p.maxInputChars)

	reqBody, err := json.Marshal(ollamaEmbedRequest{
		Model: p.model,
		Input: text,
	})
	if err != nil {
		return pgvector.Vector{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embed", bytes.NewReader(reqBody))
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

	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return pgvector.Vector{}, fmt.Errorf("ollama: empty embedding returned")
	}

	return pgvector.NewVector(result.Embeddings[0]), nil
}

// ollamaMaxConcurrency is the maximum number of parallel requests to Ollama.
// Kept low to avoid overwhelming a single local GPU.
const ollamaMaxConcurrency = 4

// EmbedBatch generates embeddings for multiple texts using Ollama's native
// batch support (/api/embed with an array input). Falls back to concurrent
// single-text requests if the batch call fails (e.g., older Ollama versions
// that don't support array input).
func (p *OllamaProvider) EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Truncate all texts before sending.
	truncated := make([]string, len(texts))
	for i, t := range texts {
		truncated[i] = truncateText(t, p.maxInputChars)
	}

	// Single text — no batch overhead.
	if len(truncated) == 1 {
		vec, err := p.Embed(ctx, truncated[0])
		if err != nil {
			return nil, err
		}
		return []pgvector.Vector{vec}, nil
	}

	// Try native batch first.
	vecs, err := p.embedBatchNative(ctx, truncated)
	if err == nil {
		return vecs, nil
	}
	slog.Debug("ollama: native batch failed, falling back to concurrent single requests", "error", err)

	return p.embedBatchConcurrent(ctx, truncated)
}

// embedBatchNative sends all texts in a single /api/embed request.
func (p *OllamaProvider) embedBatchNative(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
	reqBody, err := json.Marshal(ollamaEmbedRequest{
		Model: p.model,
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal batch request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embed", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama: create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: send batch request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ollama: batch status %d: %s", resp.StatusCode, string(body))
	}

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama: decode batch response: %w", err)
	}

	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama: expected %d embeddings, got %d", len(texts), len(result.Embeddings))
	}

	vecs := make([]pgvector.Vector, len(result.Embeddings))
	for i, emb := range result.Embeddings {
		if len(emb) == 0 {
			return nil, fmt.Errorf("ollama: empty embedding at index %d", i)
		}
		vecs[i] = pgvector.NewVector(emb)
	}
	return vecs, nil
}

// embedBatchConcurrent is the fallback: concurrent single-text requests with a semaphore.
func (p *OllamaProvider) embedBatchConcurrent(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
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

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return vecs, nil
}

