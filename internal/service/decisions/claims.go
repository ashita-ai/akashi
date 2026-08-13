package decisions

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/storage"
)

// generateClaims extracts claims from an outcome, embeds each, and stores them
// in the decision_claims table. When an LLM extractor is configured, claims are
// extracted with categories (finding, recommendation, assessment, status).
// Otherwise falls back to regex-based SplitClaims (uncategorized).
// Skips if claims already exist.
func (s *Service) generateClaims(ctx context.Context, decisionID, orgID uuid.UUID, outcome string) error {
	// Skip if claims already exist for this decision.
	exists, err := s.db.HasClaimsForDecision(ctx, decisionID, orgID)
	if err != nil {
		return fmt.Errorf("claims: check existing: %w", err)
	}
	if exists {
		return nil
	}

	type textAndCategory struct {
		text     string
		category *string // nil for regex-extracted claims
	}

	var extracted []textAndCategory

	if s.claimExtractor != nil {
		llmClaims, err := s.claimExtractor.ExtractClaims(ctx, outcome)
		if err != nil {
			// Fall back to regex on LLM failure.
			s.logger.Warn("claims: LLM extraction failed, falling back to regex",
				"decision_id", decisionID, "error", err)
		} else {
			for _, c := range llmClaims {
				cat := c.Category
				extracted = append(extracted, textAndCategory{text: c.Text, category: &cat})
			}
		}
	}

	// Fallback: regex-based extraction (no categories).
	if len(extracted) == 0 {
		for _, text := range conflicts.SplitClaims(outcome) {
			extracted = append(extracted, textAndCategory{text: text, category: nil})
		}
	}

	if len(extracted) == 0 {
		return nil
	}

	// Embed all claims in a single batch call.
	texts := make([]string, len(extracted))
	for i, e := range extracted {
		texts[i] = e.text
	}
	vecs, err := s.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("claims: embed batch: %w", err)
	}

	// Build claim records.
	claims := make([]storage.Claim, 0, len(extracted))
	for i, e := range extracted {
		if i >= len(vecs) {
			break
		}
		if err := s.validateEmbeddingDims(vecs[i]); err != nil {
			s.logger.Warn("claims: dimension mismatch, skipping claim", "decision_id", decisionID, "claim_idx", i, "error", err)
			continue
		}
		emb := vecs[i]
		claims = append(claims, storage.Claim{
			DecisionID: decisionID,
			OrgID:      orgID,
			ClaimIdx:   i,
			ClaimText:  e.text,
			Category:   e.category,
			Embedding:  &emb,
		})
	}

	if len(claims) == 0 {
		return nil
	}

	if err := s.db.InsertClaims(ctx, claims); err != nil {
		return fmt.Errorf("claims: insert: %w", err)
	}
	method := "regex"
	if s.claimExtractor != nil && extracted[0].category != nil {
		method = "llm"
	}
	s.logger.Debug("claims: generated", "decision_id", decisionID, "count", len(claims), "method", method)
	return nil
}
