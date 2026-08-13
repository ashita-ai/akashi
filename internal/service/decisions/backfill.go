package decisions

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/storage"
)

// BackfillEmbeddings generates embeddings for decisions that were stored without
// one (e.g. because the embedding provider was noop at trace time). Each decision
// is embedded, the vector is written to Postgres, and a search outbox entry is
// queued so the outbox worker can sync it to Qdrant.
//
// Returns the number of decisions backfilled. Skips silently if the embedding
// provider is noop (returns 0, nil).
func (s *Service) BackfillEmbeddings(ctx context.Context, batchSize int) (int, error) {
	return s.backfillBatch(ctx, batchSize, backfillSpec{
		find:  s.db.FindUnembeddedDecisions,
		text:  embeddingText,
		write: s.db.BackfillEmbedding,
		label: "backfill: embedded decisions",
	})
}

// BackfillOutcomeEmbeddings populates outcome_embedding for decisions that have
// embedding but no outcome_embedding (Option B). Returns the number backfilled.
func (s *Service) BackfillOutcomeEmbeddings(ctx context.Context, batchSize int) (int, error) {
	return s.backfillBatch(ctx, batchSize, backfillSpec{
		find:  s.db.FindDecisionsMissingOutcomeEmbedding,
		text:  func(d storage.UnembeddedDecision) string { return d.Outcome },
		write: s.db.BackfillOutcomeEmbedding,
		label: "backfill: outcome embeddings",
	})
}

// embeddingText builds the canonical embedding input for a decision (same
// format used by prepareTrace).
func embeddingText(d storage.UnembeddedDecision) string {
	s := d.DecisionType + ": " + d.Outcome
	if d.Reasoning != nil {
		s += " " + *d.Reasoning
	}
	return s
}

// backfillSpec parameterizes the shared backfill loop.
type backfillSpec struct {
	find  func(ctx context.Context, limit int) ([]storage.UnembeddedDecision, error)
	text  func(d storage.UnembeddedDecision) string
	write func(ctx context.Context, id uuid.UUID, orgID uuid.UUID, vec pgvector.Vector) error
	label string
}

// backfillBatch probes the embedding provider, finds records needing backfill,
// embeds them in a single batch, and writes each vector back. Shared by
// BackfillEmbeddings and BackfillOutcomeEmbeddings.
func (s *Service) backfillBatch(ctx context.Context, batchSize int, spec backfillSpec) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	decs, err := spec.find(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("%s: find: %w", spec.label, err)
	}
	if len(decs) == 0 {
		return 0, nil
	}

	texts := make([]string, len(decs))
	for i, d := range decs {
		texts[i] = spec.text(d)
	}

	vecs, err := s.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("%s: embed batch: %w", spec.label, err)
	}

	var backfilled int
	for i, d := range decs {
		if err := s.validateEmbeddingDims(vecs[i]); err != nil {
			s.logger.Warn(spec.label+": dimension mismatch, skipping", "decision_id", d.ID, "error", err)
			continue
		}
		if err := spec.write(ctx, d.ID, d.OrgID, vecs[i]); err != nil {
			s.logger.Warn(spec.label+": update failed", "decision_id", d.ID, "error", err)
			continue
		}
		backfilled++
	}

	if backfilled > 0 {
		s.logger.Info(spec.label, "count", backfilled, "batch", len(decs))
	}
	return backfilled, nil
}

// BackfillClaims generates sentence-level claim embeddings for decisions that
// have embeddings but no claims yet. Returns the number of decisions processed.
func (s *Service) BackfillClaims(ctx context.Context, batchSize int) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	refs, err := s.db.FindDecisionIDsMissingClaims(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("backfill claims: find: %w", err)
	}
	if len(refs) == 0 {
		return 0, nil
	}

	var backfilled int
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return backfilled, ctx.Err()
		default:
		}
		// Fetch the decision outcome.
		d, err := s.db.GetDecisionForScoring(ctx, ref.ID, ref.OrgID)
		if err != nil {
			s.logger.Warn("backfill claims: get decision failed", "decision_id", ref.ID, "error", err)
			continue
		}
		if err := s.generateClaims(ctx, ref.ID, ref.OrgID, d.Outcome); err != nil {
			s.logger.Warn("backfill claims: generate failed", "decision_id", ref.ID, "error", err)
			continue
		}
		backfilled++
	}

	if backfilled > 0 {
		s.logger.Info("backfill: claims generated", "count", backfilled, "batch", len(refs))
	}
	return backfilled, nil
}

// RetryFailedClaimEmbeddings re-attempts claim embedding generation for decisions
// that failed previously and are eligible for retry (exponential backoff, capped
// at maxAttempts). On success, clears the failure state and triggers conflict
// scoring. On failure, increments the attempt counter for longer backoff.
// Returns the number of decisions successfully retried.
func (s *Service) RetryFailedClaimEmbeddings(ctx context.Context, batchSize, maxAttempts int) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	refs, err := s.db.FindRetriableClaimFailures(ctx, maxAttempts, batchSize)
	if err != nil {
		return 0, fmt.Errorf("retry claims: find: %w", err)
	}
	if len(refs) == 0 {
		return 0, nil
	}

	var retried int
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return retried, ctx.Err()
		default:
		}

		d, err := s.db.GetDecisionForScoring(ctx, ref.ID, ref.OrgID)
		if err != nil {
			s.logger.Warn("retry claims: get decision failed", "decision_id", ref.ID, "error", err)
			continue
		}

		if err := s.generateClaims(ctx, ref.ID, ref.OrgID, d.Outcome); err != nil {
			s.logger.Warn("retry claims: generate failed", "decision_id", ref.ID, "error", err)
			s.claimEmbeddingFailures.Add(ctx, 1, metric.WithAttributes(
				attribute.Int("attempt_number", ref.Attempts+1),
			))
			if markErr := s.db.MarkClaimEmbeddingFailed(ctx, ref.ID, ref.OrgID); markErr != nil {
				s.logger.Error("retry claims: failed to mark failure", "decision_id", ref.ID, "error", markErr)
			}
			continue
		}

		if err := s.db.ClearClaimEmbeddingFailure(ctx, ref.ID, ref.OrgID); err != nil {
			s.logger.Error("retry claims: failed to clear failure state", "decision_id", ref.ID, "error", err)
		}

		if s.conflictScorer != nil {
			s.conflictScorer.ScoreForDecision(ctx, ref.ID, ref.OrgID)
		}
		retried++
	}

	if retried > 0 {
		s.logger.Info("retry claims: succeeded", "count", retried, "batch", len(refs))
	}
	return retried, nil
}

// isDuplicateKey delegates to the storage backend to check for unique constraint
// violations (Postgres 23505, SQLite CONSTRAINT_UNIQUE, etc.).
func (s *Service) isDuplicateKey(err error) bool {
	return s.db.IsDuplicateKey(err)
}
