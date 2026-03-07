package conflicts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPCrossEncoder_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/score", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crossEncoderResponse{Score: 0.85})
	}))
	defer srv.Close()

	ce := NewHTTPCrossEncoder(srv.URL)
	score, err := ce.ScoreContradiction(context.Background(), "chose Redis", "chose Memcached")
	require.NoError(t, err)
	assert.InDelta(t, 0.85, score, 1e-9)
}

func TestHTTPCrossEncoder_LowScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crossEncoderResponse{Score: 0.12})
	}))
	defer srv.Close()

	ce := NewHTTPCrossEncoder(srv.URL)
	score, err := ce.ScoreContradiction(context.Background(), "use REST", "use REST too")
	require.NoError(t, err)
	assert.InDelta(t, 0.12, score, 1e-9)
}

func TestHTTPCrossEncoder_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ce := NewHTTPCrossEncoder(srv.URL)
	_, err := ce.ScoreContradiction(context.Background(), "a", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestHTTPCrossEncoder_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(crossEncoderTimeout + 2*time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crossEncoderResponse{Score: 0.5})
	}))
	defer srv.Close()

	ce := NewHTTPCrossEncoder(srv.URL)
	_, err := ce.ScoreContradiction(context.Background(), "a", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-encoder")
}

func TestHTTPCrossEncoder_MalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	ce := NewHTTPCrossEncoder(srv.URL)
	_, err := ce.ScoreContradiction(context.Background(), "a", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode response")
}

func TestHTTPCrossEncoder_RequestFormat(t *testing.T) {
	var received crossEncoderRequest
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &received))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crossEncoderResponse{Score: 0.5})
	}))
	defer srv.Close()

	ce := NewHTTPCrossEncoder(srv.URL)
	_, err := ce.ScoreContradiction(context.Background(), "chose Redis for caching", "chose Memcached for caching")
	require.NoError(t, err)

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, "chose Redis for caching", received.TextA)
	assert.Equal(t, "chose Memcached for caching", received.TextB)
}

func TestHTTPCrossEncoder_ConnectionRefused(t *testing.T) {
	ce := NewHTTPCrossEncoder("http://127.0.0.1:1") // nothing listening
	_, err := ce.ScoreContradiction(context.Background(), "a", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request failed")
}

func TestHTTPCrossEncoder_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crossEncoderResponse{Score: 0.5})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	ce := NewHTTPCrossEncoder(srv.URL)
	_, err := ce.ScoreContradiction(ctx, "a", "b")
	require.Error(t, err)
}
