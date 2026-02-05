package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pgvector/pgvector-go"
)

// OllamaProvider generates embeddings using a local Ollama server.
type OllamaProvider struct {
	baseURL    string
	model      string
	httpClient *http.Client
	dimensions int
	padTo      int
}

// NewOllamaProvider creates a provider that calls Ollama's embedding API.
// Model should be an embedding model like "mxbai-embed-large" or "nomic-embed-text".
// If padTo > dimensions, vectors are zero-padded for schema compatibility.
func NewOllamaProvider(baseURL, model string, dimensions, padTo int) *OllamaProvider {
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
		padTo:      padTo,
	}
}

// Dimensions returns the output vector size (after padding if configured).
func (p *OllamaProvider) Dimensions() int {
	if p.padTo > p.dimensions {
		return p.padTo
	}
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
	defer resp.Body.Close()

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

	// Pad if needed for schema compatibility.
	vec := result.Embedding
	if p.padTo > len(vec) {
		padded := make([]float32, p.padTo)
		copy(padded, vec)
		vec = padded
	}

	return pgvector.NewVector(vec), nil
}

// EmbedBatch generates embeddings for multiple texts.
// Ollama doesn't have a native batch API, so we call sequentially.
func (p *OllamaProvider) EmbedBatch(ctx context.Context, texts []string) ([]pgvector.Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vecs := make([]pgvector.Vector, len(texts))
	for i, text := range texts {
		vec, err := p.Embed(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("ollama: batch item %d: %w", i, err)
		}
		vecs[i] = vec
	}
	return vecs, nil
}
