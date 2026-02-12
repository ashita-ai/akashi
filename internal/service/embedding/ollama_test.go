package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaProvider(t *testing.T) {
	// Mock Ollama server returning 1024-dim embeddings via /api/embed.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ollamaEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Determine how many embeddings to return based on input type.
		var count int
		switch v := req.Input.(type) {
		case string:
			count = 1
		case []any:
			count = len(v)
		default:
			http.Error(w, "unexpected input type", http.StatusBadRequest)
			return
		}

		embeddings := make([][]float32, count)
		for i := range embeddings {
			vec := make([]float32, 1024)
			for j := range vec {
				vec[j] = float32(j) * 0.001
			}
			embeddings[i] = vec
		}
		if err := json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: embeddings}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	t.Run("dimensions", func(t *testing.T) {
		p := NewOllamaProvider(server.URL, "test-model", 1024)
		if p.Dimensions() != 1024 {
			t.Errorf("expected 1024, got %d", p.Dimensions())
		}
	})

	t.Run("embed single", func(t *testing.T) {
		p := NewOllamaProvider(server.URL, "test-model", 1024)
		vec, err := p.Embed(context.Background(), "test text")
		if err != nil {
			t.Fatal(err)
		}
		slice := vec.Slice()
		if len(slice) != 1024 {
			t.Errorf("expected 1024-dim vector, got %d", len(slice))
		}
		if slice[0] != 0.0 {
			t.Errorf("expected first element to be 0.0, got %f", slice[0])
		}
		if slice[100] != 0.1 {
			t.Errorf("expected element 100 to be 0.1, got %f", slice[100])
		}
	})

	t.Run("embed batch", func(t *testing.T) {
		p := NewOllamaProvider(server.URL, "test-model", 1024)
		vecs, err := p.EmbedBatch(context.Background(), []string{"a", "b", "c"})
		if err != nil {
			t.Fatal(err)
		}
		if len(vecs) != 3 {
			t.Errorf("expected 3 vectors, got %d", len(vecs))
		}
		for i, vec := range vecs {
			if len(vec.Slice()) != 1024 {
				t.Errorf("vector %d: expected 1024-dim, got %d", i, len(vec.Slice()))
			}
		}
	})

	t.Run("embed batch empty", func(t *testing.T) {
		p := NewOllamaProvider(server.URL, "test-model", 1024)
		vecs, err := p.EmbedBatch(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if vecs != nil {
			t.Errorf("expected nil, got %v", vecs)
		}
	})
}

func TestOllamaProviderErrors(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		defer server.Close()

		p := NewOllamaProvider(server.URL, "test-model", 1024)
		_, err := p.Embed(context.Background(), "test")
		if err == nil {
			t.Error("expected error, got nil")
		}
	})

	t.Run("empty embedding", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: nil})
		}))
		defer server.Close()

		p := NewOllamaProvider(server.URL, "test-model", 1024)
		_, err := p.Embed(context.Background(), "test")
		if err == nil {
			t.Error("expected error for empty embedding, got nil")
		}
	})

	t.Run("invalid json response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer server.Close()

		p := NewOllamaProvider(server.URL, "test-model", 1024)
		_, err := p.Embed(context.Background(), "test")
		if err == nil {
			t.Error("expected error for invalid json, got nil")
		}
	})
}

func TestTruncateText(t *testing.T) {
	t.Run("short text unchanged", func(t *testing.T) {
		got := truncateText("hello world", 100)
		if got != "hello world" {
			t.Errorf("expected 'hello world', got %q", got)
		}
	})

	t.Run("exact limit unchanged", func(t *testing.T) {
		text := "hello"
		got := truncateText(text, 5)
		if got != "hello" {
			t.Errorf("expected 'hello', got %q", got)
		}
	})

	t.Run("truncates at word boundary", func(t *testing.T) {
		text := "the quick brown fox jumps over the lazy dog"
		got := truncateText(text, 20)
		if got != "the quick brown fox" {
			t.Errorf("expected 'the quick brown fox', got %q", got)
		}
	})

	t.Run("hard truncate when no spaces", func(t *testing.T) {
		text := strings.Repeat("a", 30)
		got := truncateText(text, 10)
		if len(got) != 10 {
			t.Errorf("expected length 10, got %d", len(got))
		}
	})

	t.Run("empty text", func(t *testing.T) {
		got := truncateText("", 100)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}
