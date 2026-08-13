//go:build !lite

package conflicts

import (
	"context"
	"fmt"
	"strings"

	"github.com/ashita-ai/akashi/internal/compact"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// validatorSideDecision maps the side token the validator named on its REPLACES
// line back onto the scored pair.
//
// The mapping is fixed by the ValidateInput literal in scoreForDecision:
// OutcomeA / TypeA / AgentA / CreatedA / FullOutcomeA / BranchA / TicketRefsA
// are ALL built from d, and every *B field from cand. So "A" is d and "B" is
// cand. Any change to that literal must change this function with it; there is
// no other place the correspondence is recorded.
func validatorSideDecision(side string, d, cand model.Decision) (superseding, superseded model.Decision, ok bool) {
	switch side {
	case "A":
		return d, cand, true
	case "B":
		return cand, d, true
	default:
		return model.Decision{}, model.Decision{}, false
	}
}

// recordSupersedesSuggestion writes the detector-inferred supersedes link for a
// suppressed same-agent same-ticket pair so akashi_check can surface it on the
// agent's next call (PR-2 of #708, see issue #710).
//
// This path runs in the pre-LLM structural-filter block: there is no judge
// verdict, so recorded order is the ONLY direction signal available and the
// reason string says so explicitly. Do not borrow the LLM path's language here
// — the attribution has to stay honest about what produced it.
func (s *Scorer) recordSupersedesSuggestion(ctx context.Context, d, cand model.Decision, topicSim float64, ticket string) {
	superseding, superseded := d, cand
	if cand.ValidFrom.After(d.ValidFrom) {
		superseding, superseded = cand, d
	}
	s.insertSupersedesSuggestion(ctx, superseding, superseded, topicSim,
		supersedesSourceSameTicket,
		fmt.Sprintf("same agent %q, same ticket %q; direction from recorded order (no validator verdict on this path)",
			d.AgentID, ticket))
}

// recordSupersedesSuggestionFromValidator records the supersedes link the LLM
// validator inferred when it classified a pair as a supersession. A supersession
// is a lifecycle event — one decision explicitly replaces another — not a
// standing disagreement between live positions, so it is surfaced as a supersedes
// suggestion (which drives the supersedes_id link on the agent's next
// akashi_check, issue #710) instead of being opened as a conflict a human must
// triage.
//
// Direction comes from the judge's REPLACES line, never from ValidFrom. The
// prompt refuses recency as evidence ("every candidate pair has a time order,
// so ordering alone means nothing") and re-deriving direction from the clock
// here reintroduced exactly that, breaking on backdated traces, late-filed
// decisions, and reverts. When the judge's direction and recorded order
// disagree, that is signal about the trace, not an error to resolve in favour
// of the clock: the row is written with the judge's direction under a distinct
// suggested_by so the two populations stay separable.
func (s *Scorer) recordSupersedesSuggestionFromValidator(ctx context.Context, d, cand model.Decision, topicSim float64, result ValidationResult) {
	superseding, superseded, ok := validatorSideDecision(result.SupersedingSide, d, cand)
	if !ok {
		// ParseValidatorResponse guarantees a supersession verdict carries a
		// side token — it downgrades one that does not. So an empty or
		// unrecognised side here means the ValidationResult did not come from
		// the parser: an in-process validator, or a test double constructing
		// the struct directly.
		//
		// Direction IS the content of this record, so refusing to write it is
		// the only honest option; falling back to ValidFrom is the clock
		// inference this function exists to remove. Logged at error rather than
		// warn because it is a contract violation, not a transient write
		// failure — but it still returns rather than propagating, because this
		// is a fire-and-forget path inside the scoring loop and the documented
		// contract is that suggestion writes never block scoring. Loud with no
		// side effect beats silent with a guessed direction.
		s.metrics.supersessionSideMissing.Add(ctx, 1)
		s.logger.Error("conflict scorer: supersession verdict carried no superseding side, no supersedes suggestion written",
			"decision_a", d.ID, "decision_b", cand.ID,
			"side", result.SupersedingSide,
			"validator", s.validatorLabel)
		return
	}

	// The judge quoted replacement language when it could; its one-sentence
	// explanation is the fallback. Either way the reason carries the judge's
	// own finding instead of a template rendered from an inference we just made.
	detail := result.ReplacementEvidence
	if detail == "" {
		detail = result.Explanation
	}
	detail = compact.Truncate(strings.TrimSpace(detail), supersedesReasonMaxDetail)

	// Decision IDs are included because agent IDs do not disambiguate the sides
	// of a same-agent pair, which is the common case for this detector.
	reason := fmt.Sprintf("LLM validator: %q's decision %s replaces %q's decision %s — %s",
		superseding.AgentID, superseding.ID.String()[:8],
		superseded.AgentID, superseded.ID.String()[:8], detail)

	suggestedBy := supersedesSourceLLM
	// Disagreement is strict: equal timestamps mean the clock has no opinion,
	// not that it dissents.
	if superseded.ValidFrom.After(superseding.ValidFrom) {
		suggestedBy = supersedesSourceLLMBackdated
		reason = "judge direction disagrees with recorded order (the retired decision has the later valid_from): " + reason
		s.metrics.supersessionDirectionDisagreement.Add(ctx, 1)
		s.logger.Warn("conflict scorer: supersedes direction from validator disagrees with recorded order",
			"superseding_id", superseding.ID, "superseded_id", superseded.ID,
			"superseding_valid_from", superseding.ValidFrom,
			"superseded_valid_from", superseded.ValidFrom,
			"side", result.SupersedingSide,
			"validator", s.validatorLabel)
	}

	s.insertSupersedesSuggestion(ctx, superseding, superseded, topicSim, suggestedBy, reason)
}

// insertSupersedesSuggestion is the shared core behind the supersedes-suggestion
// callers. Direction is the CALLER's decision — this function does not infer it
// — because the two callers derive it from different evidence (the judge's
// REPLACES line vs recorded order on a path with no judge). It clamps topicSim
// into the [0,1] confidence contract (cosine similarity is mathematically in
// [-1,1] but the API bounds confidence to [0,1]) and writes fire-and-forget: a
// write failure is logged at warn and never blocks scoring, because the pair has
// already been filtered or redirected away from the open queue by the time this
// runs.
func (s *Scorer) insertSupersedesSuggestion(ctx context.Context, superseding, superseded model.Decision, topicSim float64, suggestedBy, reason string) {
	switch {
	case topicSim < 0:
		topicSim = 0
	case topicSim > 1:
		topicSim = 1
	}
	conf := float32(topicSim)
	inverseExists, err := s.db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID:         superseding.OrgID,
		SupersedingID: superseding.ID,
		SupersededID:  superseded.ID,
		SuggestedBy:   suggestedBy,
		Confidence:    &conf,
		Reason:        reason,
	})
	if err != nil {
		s.logger.Warn("conflict scorer: insert supersedes suggestion failed",
			"superseding_id", superseding.ID, "superseded_id", superseded.ID,
			"suggested_by", suggestedBy, "error", err)
		return
	}
	if inverseExists {
		// The opposite direction is already recorded for this pair. Since the
		// judge now decides direction and is sampled, this means two passes over
		// the same pair disagreed about which decision retired which. Nothing was
		// written. Surface it at WARN rather than resolving it silently — a
		// direction picked by whichever pass ran last is exactly the guessed link
		// this change exists to stop recording.
		s.metrics.supersedesInverseSuppressed.Add(ctx, 1)
		s.logger.Warn("conflict scorer: supersedes suggestion contradicts an existing inverse link, not recorded",
			"superseding_id", superseding.ID, "superseded_id", superseded.ID,
			"suggested_by", suggestedBy)
	}
}
