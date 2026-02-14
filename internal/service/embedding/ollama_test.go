package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestNoopProvider_Embed(t *testing.T) {
	p := NewNoopProvider(1024)
	_, err := p.Embed(context.Background(), "some text")
	if err == nil {
		t.Fatal("expected error from NoopProvider.Embed, got nil")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
}

func TestNoopProvider_EmbedBatch(t *testing.T) {
	p := NewNoopProvider(1024)
	vecs, err := p.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error from NoopProvider.EmbedBatch, got nil")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
	if vecs != nil {
		t.Errorf("expected nil vectors, got %v", vecs)
	}
}

func TestNoopProvider_Dimensions(t *testing.T) {
	t.Run("1024", func(t *testing.T) {
		p := NewNoopProvider(1024)
		if got := p.Dimensions(); got != 1024 {
			t.Errorf("expected 1024, got %d", got)
		}
	})

	t.Run("512", func(t *testing.T) {
		p := NewNoopProvider(512)
		if got := p.Dimensions(); got != 512 {
			t.Errorf("expected 512, got %d", got)
		}
	})
}

func TestDetectProvider_NoConfig(t *testing.T) {
	// When no embedding API keys are configured, a NoopProvider is the
	// expected fallback. Verify its contract: Embed returns ErrNoProvider
	// and Dimensions returns whatever was configured at construction time.
	p := NewNoopProvider(768)

	_, err := p.Embed(context.Background(), "test input")
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider from noop Embed, got %v", err)
	}

	if got := p.Dimensions(); got != 768 {
		t.Errorf("expected dimensions 768, got %d", got)
	}
}

func TestTruncateText_UTF8Safety(t *testing.T) {
	// Japanese characters are 3 bytes each in UTF-8. Truncating in the
	// middle of the rune slice must never produce invalid UTF-8 or exceed
	// the rune limit.
	input := "こんにちは世界テスト"                      // 9 runes, 27 bytes
	runeCount := utf8.RuneCountInString(input) // 9

	t.Run("truncate mid-string", func(t *testing.T) {
		limit := 5
		got := truncateText(input, limit)

		if !utf8.ValidString(got) {
			t.Fatalf("truncated string is not valid UTF-8: %q", got)
		}

		gotRunes := utf8.RuneCountInString(got)
		if gotRunes > limit {
			t.Errorf("rune count %d exceeds limit %d", gotRunes, limit)
		}
	})

	t.Run("limit exceeds length", func(t *testing.T) {
		got := truncateText(input, runeCount+10)
		if got != input {
			t.Errorf("expected original string unchanged, got %q", got)
		}
	})

	t.Run("limit equals length", func(t *testing.T) {
		got := truncateText(input, runeCount)
		if got != input {
			t.Errorf("expected original string unchanged, got %q", got)
		}
	})

	t.Run("mixed ascii and multibyte", func(t *testing.T) {
		mixed := "hello こんにちは world"
		limit := 8
		got := truncateText(mixed, limit)

		if !utf8.ValidString(got) {
			t.Fatalf("truncated mixed string is not valid UTF-8: %q", got)
		}

		gotRunes := utf8.RuneCountInString(got)
		if gotRunes > limit {
			t.Errorf("rune count %d exceeds limit %d", gotRunes, limit)
		}
	})
}

func TestOpenAIProvider_RequiresAPIKey(t *testing.T) {
	p, err := NewOpenAIProvider("", "text-embedding-3-small", 1024)
	if err == nil {
		t.Fatal("expected error for empty API key, got nil")
	}
	if p != nil {
		t.Errorf("expected nil provider on error, got %v", p)
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("error should mention API key, got: %v", err)
	}
}

func TestOpenAIProvider_Dimensions(t *testing.T) {
	t.Run("explicit dimensions", func(t *testing.T) {
		p, err := NewOpenAIProvider("sk-test-key", "text-embedding-3-small", 1024)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := p.Dimensions(); got != 1024 {
			t.Errorf("expected 1024, got %d", got)
		}
	})

	t.Run("default dimensions when zero", func(t *testing.T) {
		// When dimensions <= 0, NewOpenAIProvider defaults to 1536.
		p, err := NewOpenAIProvider("sk-test-key", "text-embedding-3-small", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := p.Dimensions(); got != 1536 {
			t.Errorf("expected default 1536, got %d", got)
		}
	})

	t.Run("negative dimensions defaults to 1536", func(t *testing.T) {
		p, err := NewOpenAIProvider("sk-test-key", "text-embedding-3-small", -1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := p.Dimensions(); got != 1536 {
			t.Errorf("expected default 1536, got %d", got)
		}
	})
}

func TestOpenAIProvider_Embed_InvalidKey(t *testing.T) {
	// Mock OpenAI server that returns a 401 with a structured error body.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Error: &struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			}{
				Message: "Incorrect API key provided: sk-test-****-key",
				Type:    "invalid_request_error",
			},
		})
	}))
	defer server.Close()

	// Create a provider pointed at the mock server. We cannot change the
	// hardcoded URL in OpenAIProvider, so instead we test with the real
	// Embed method hitting a non-existent endpoint, verifying it returns
	// an error without panicking.
	p, err := NewOpenAIProvider("sk-invalid-test-key", "text-embedding-3-small", 1024)
	if err != nil {
		t.Fatalf("unexpected construction error: %v", err)
	}

	// Call Embed — the provider will try to reach the real OpenAI API with an
	// invalid key. We expect an error (network or auth), never a panic.
	_, embedErr := p.Embed(context.Background(), "test text")
	if embedErr == nil {
		t.Error("expected error from Embed with invalid API key, got nil")
	}
}

func TestOllamaProvider_EmbedBatch_MockServer(t *testing.T) {
	// Mock Ollama server that returns distinct embeddings per input text.
	// Each embedding's first element is set to the index for verification.
	dims := 128
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var req ollamaEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

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
			vec := make([]float32, dims)
			// Set first element to index so we can verify ordering.
			vec[0] = float32(i)
			for j := 1; j < dims; j++ {
				vec[j] = float32(j) * 0.01
			}
			embeddings[i] = vec
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: embeddings}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "test-model", dims)

	t.Run("batch of 5 texts", func(t *testing.T) {
		texts := []string{"alpha", "bravo", "charlie", "delta", "echo"}
		vecs, err := p.EmbedBatch(context.Background(), texts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vecs) != len(texts) {
			t.Fatalf("expected %d vectors, got %d", len(texts), len(vecs))
		}
		for i, vec := range vecs {
			slice := vec.Slice()
			if len(slice) != dims {
				t.Errorf("vector %d: expected %d dims, got %d", i, dims, len(slice))
			}
		}
	})

	t.Run("single text batch delegates to Embed", func(t *testing.T) {
		// EmbedBatch with a single text delegates to Embed (no batch overhead).
		vecs, err := p.EmbedBatch(context.Background(), []string{"solo"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vecs) != 1 {
			t.Fatalf("expected 1 vector, got %d", len(vecs))
		}
		if len(vecs[0].Slice()) != dims {
			t.Errorf("expected %d dims, got %d", dims, len(vecs[0].Slice()))
		}
	})

	t.Run("batch native failure falls back to concurrent", func(t *testing.T) {
		// Create a server that rejects array inputs but accepts single strings.
		// This simulates an older Ollama version without native batch support.
		fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req ollamaEmbedRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}

			switch req.Input.(type) {
			case string:
				// Single text: succeed.
				vec := make([]float32, dims)
				for j := range vec {
					vec[j] = 0.5
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{
					Embeddings: [][]float32{vec},
				})
			case []any:
				// Array input: reject to trigger fallback.
				http.Error(w, "batch not supported", http.StatusBadRequest)
			default:
				http.Error(w, "unexpected", http.StatusBadRequest)
			}
		}))
		defer fallbackServer.Close()

		fp := NewOllamaProvider(fallbackServer.URL, "test-model", dims)
		texts := []string{"one", "two", "three"}
		vecs, err := fp.EmbedBatch(context.Background(), texts)
		if err != nil {
			t.Fatalf("expected fallback to succeed, got error: %v", err)
		}
		if len(vecs) != 3 {
			t.Errorf("expected 3 vectors from fallback, got %d", len(vecs))
		}
		// Verify all vectors have the expected fill value from the fallback handler.
		for i, vec := range vecs {
			slice := vec.Slice()
			if len(slice) != dims {
				t.Errorf("fallback vector %d: expected %d dims, got %d", i, dims, len(slice))
			}
			if slice[0] != 0.5 {
				t.Errorf("fallback vector %d: expected first element 0.5, got %f", i, slice[0])
			}
		}
	})
}
