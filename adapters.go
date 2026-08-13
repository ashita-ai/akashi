package akashi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/config"
	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/embedding"
)

// decisionHookAdapter wraps an akashi.EventHook to satisfy server.DecisionHook.
// It converts internal model types to public akashi types at the boundary.
type decisionHookAdapter struct {
	hook EventHook
}

func (a *decisionHookAdapter) OnDecisionTraced(ctx context.Context, d model.Decision) error {
	return a.hook.OnDecisionTraced(ctx, toPublicDecision(d))
}

func (a *decisionHookAdapter) OnConflictDetected(ctx context.Context, c model.DecisionConflict) error {
	return a.hook.OnConflictDetected(ctx, toPublicConflict(c))
}

// externalScorerAdapter wraps an akashi.ConflictScorer to satisfy conflicts.PairwiseScorer.
type externalScorerAdapter struct {
	scorer ConflictScorer
}

func (a *externalScorerAdapter) ScorePair(ctx context.Context, da, db model.Decision) (float32, string, error) {
	result, err := a.scorer.Score(ctx, toPublicDecision(da), toPublicDecision(db))
	if err != nil {
		return 0, "", err
	}
	return result.Score, result.Explanation, nil
}

// searcherAdapter wraps an akashi.Searcher to satisfy search.Searcher.
// Converts between public SearchFilters/SearchResult and internal model types.
type searcherAdapter struct {
	s Searcher
}

func (a *searcherAdapter) Search(ctx context.Context, orgID uuid.UUID, emb []float32, filters model.QueryFilters, limit int) ([]search.Result, error) {
	pubFilters := SearchFilters{
		AgentIDs:      filters.AgentIDs,
		DecisionType:  filters.DecisionType,
		ConfidenceMin: filters.ConfidenceMin,
		SessionID:     filters.SessionID,
		Tool:          filters.Tool,
		Model:         filters.Model,
		Project:       filters.Project,
	}
	results, err := a.s.Search(ctx, orgID, emb, pubFilters, limit)
	if err != nil {
		return nil, err
	}
	out := make([]search.Result, len(results))
	for i, r := range results {
		out[i] = search.Result{DecisionID: r.DecisionID, Score: r.Score}
	}
	return out, nil
}

func (a *searcherAdapter) Healthy(ctx context.Context) error {
	return a.s.Healthy(ctx)
}

// authHelperImpl implements akashi.AuthHelper using an internal server.RoleMiddlewareFn.
// Constructed in the route registrar adapter closure; bridges the public interface
// to the internal RBAC middleware without importing server from enterprise code.
type authHelperImpl struct {
	roleFn server.RoleMiddlewareFn
}

func (a *authHelperImpl) RequireRole(role Role) func(http.Handler) http.Handler {
	return a.roleFn(model.AgentRole(role))
}

func newEmbeddingProvider(cfg config.Config, logger *slog.Logger) embedding.Provider {
	dims := cfg.EmbeddingDimensions

	switch cfg.EmbeddingProvider {
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			logger.Error("OPENAI_API_KEY required when AKASHI_EMBEDDING_PROVIDER=openai")
			return embedding.NewNoopProvider(dims)
		}
		logger.Info("embedding provider: openai", "model", cfg.EmbeddingModel, "dimensions", dims)
		p, err := embedding.NewOpenAIProvider(cfg.OpenAIAPIKey.Value(), cfg.EmbeddingModel, dims)
		if err != nil {
			logger.Error("openai provider init failed", "error", err)
			return embedding.NewNoopProvider(dims)
		}
		return p
	case "ollama":
		logger.Info("embedding provider: ollama", "url", cfg.OllamaURL, "model", cfg.OllamaModel, "dimensions", dims)
		return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, dims)
	case "noop":
		logger.Info("embedding provider: noop (semantic search disabled)")
		return embedding.NewNoopProvider(dims)
	case "auto":
		fallthrough
	default:
		if ollamaReachable(cfg.OllamaURL) {
			logger.Info("embedding provider: ollama (auto-detected)", "url", cfg.OllamaURL, "model", cfg.OllamaModel, "dimensions", dims)
			return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, dims)
		}
		if cfg.OpenAIAPIKey != "" {
			logger.Info("embedding provider: openai (auto-detected)", "model", cfg.EmbeddingModel, "dimensions", dims)
			p, err := embedding.NewOpenAIProvider(cfg.OpenAIAPIKey.Value(), cfg.EmbeddingModel, dims)
			if err != nil {
				logger.Error("openai provider init failed", "error", err)
				return embedding.NewNoopProvider(dims)
			}
			return p
		}
		logger.Warn("no embedding provider available, using noop (semantic search disabled)")
		return embedding.NewNoopProvider(dims)
	}
}

func newConflictValidator(cfg config.Config, logger *slog.Logger) conflicts.Validator {
	if cfg.ConflictLLMModel != "" {
		logger.Info("conflict validator: ollama", "model", cfg.ConflictLLMModel, "url", cfg.OllamaURL, "num_threads", cfg.ConflictLLMThreads)
		return conflicts.NewOllamaValidator(cfg.OllamaURL, cfg.ConflictLLMModel, cfg.ConflictLLMThreads)
	}
	if cfg.OpenAIAPIKey != "" {
		logger.Info("conflict validator: openai", "model", cfg.ConflictOpenAIModel, "timeout", cfg.ConflictLLMTimeout)
		return conflicts.NewOpenAIValidator(cfg.OpenAIAPIKey.Value(), cfg.ConflictOpenAIModel,
			conflicts.WithRequestTimeout(cfg.ConflictLLMTimeout))
	}
	logger.Info("conflict validator: noop (no LLM configured, embedding-only conflicts)")
	return conflicts.NoopValidator{}
}

// newClaimExtractor creates an LLM-backed claim extractor when configured.
// Returns nil when LLM claim extraction is disabled or no LLM is available.
func newClaimExtractor(cfg config.Config, logger *slog.Logger) conflicts.ClaimExtractor {
	if !cfg.ClaimExtractionLLM {
		return nil
	}
	if cfg.ConflictLLMModel != "" {
		logger.Info("claim extractor: ollama", "model", cfg.ConflictLLMModel, "url", cfg.OllamaURL)
		return conflicts.NewOllamaExtractor(cfg.OllamaURL, cfg.ConflictLLMModel, cfg.ConflictLLMThreads)
	}
	if cfg.OpenAIAPIKey != "" {
		logger.Info("claim extractor: openai (gpt-4o-mini)")
		return conflicts.NewOpenAIExtractor(cfg.OpenAIAPIKey.Value(), "gpt-4o-mini")
	}
	logger.Warn("claim extraction LLM requested but no LLM configured, using regex fallback")
	return nil
}

func ollamaReachable(baseURL string) bool {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
