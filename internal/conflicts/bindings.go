//go:build !lite

package conflicts

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// scoringMethodBinding marks conflicts found by joining declared bindings
// rather than by scoring text. Keeping it distinct from the LLM methods is what
// lets the two be measured separately: a binding conflict has no
// false-positive rate to estimate, and averaging it into the judge's numbers
// would flatter them.
const scoringMethodBinding = "binding_v1"

// scoreBindings finds decisions that bind one of this decision's parameters to
// a different value, and records each as a conflict.
//
// This is the exact half of detection. Where the judge is screening at a ~3%
// base rate and every verdict carries a false-positive rate, a binding
// conflict is a join: the two decisions declared the same parameter and
// declared different values for it, and there is nothing to be uncertain
// about. Measuring the shape of the corpus's 93 gold contradictions found 27%
// take this form; the rest have no key to join on and are left to the judge.
//
// It runs before candidate scoring and does not depend on it: bindings conflict
// regardless of embedding similarity, which matters because the pairs that
// carry a declared parameter are not necessarily the pairs that look alike.
func (s *Scorer) scoreBindings(ctx context.Context, d model.Decision, orgID uuid.UUID) {
	bindings, err := s.db.GetBindingsByDecision(ctx, d.ID, orgID)
	if err != nil {
		s.logger.Warn("conflict scorer: load bindings failed", "error", err, "decision", d.ID)
		return
	}
	if len(bindings) == 0 {
		return
	}

	conflicts, err := s.db.FindConflictingBindings(ctx, d.ID, orgID, bindings)
	if err != nil {
		s.logger.Warn("conflict scorer: binding conflict query failed", "error", err, "decision", d.ID)
		return
	}

	// Index the decision's own bindings so the recorded conflict can name the
	// value as the agent wrote it rather than its canonical key.
	own := make(map[string]model.Binding, len(bindings))
	for _, b := range bindings {
		own[b.ParameterKey] = b
	}

	for _, c := range conflicts {
		mine, ok := own[canonicalOf(c.Parameter, own)]
		if !ok {
			continue
		}
		explanation := fmt.Sprintf(
			"%s set %s to %q; %s set the same parameter to %q. Both decisions are current, so a reader has no way to know which value holds.",
			d.AgentID, mine.Parameter, mine.Value, c.OtherAgentID, c.OtherValue)

		kind := model.ConflictKindCrossAgent
		if d.AgentID == c.OtherAgentID {
			kind = model.ConflictKindSelfContradiction
		}

		conflict := model.DecisionConflict{
			ConflictKind:  kind,
			DecisionAID:   d.ID,
			DecisionBID:   c.OtherDecisionID,
			OrgID:         orgID,
			AgentA:        d.AgentID,
			AgentB:        c.OtherAgentID,
			DecisionTypeA: d.DecisionType,
			DecisionTypeB: d.DecisionType,
			OutcomeA:      d.Outcome,
			OutcomeB:      fmt.Sprintf("%s = %s", mine.Parameter, c.OtherValue),
			ScoringMethod: scoringMethodBinding,
			// A declared collision is certain, so the scores that express
			// uncertainty about a text pair are deliberately left unset rather
			// than filled with a fabricated 1.0.
			Relationship: ptr("contradiction"),
			Explanation:  &explanation,
			Category:     ptr("factual"),
			Status:       "open",
			ProjectA:     d.Project,
			ProjectB:     c.OtherProject,
		}

		if _, err := s.db.InsertScoredConflict(ctx, conflict); err != nil {
			s.logger.Warn("conflict scorer: insert binding conflict failed",
				"error", err, "decision_a", d.ID, "decision_b", c.OtherDecisionID, "parameter", mine.Parameter)
			continue
		}
		s.metrics.bindingConflicts.Add(ctx, 1)
		s.logger.Info("conflict scorer: binding conflict",
			"parameter", mine.Parameter, "value_a", mine.Value, "value_b", c.OtherValue,
			"decision_a", d.ID, "decision_b", c.OtherDecisionID)
	}
}

// canonicalOf maps a stored parameter string back to the canonical key used in
// the caller's own binding map. The query returns the OTHER decision's spelling
// of the parameter, which may differ in case or separators from ours even
// though both canonicalize identically.
func canonicalOf(parameter string, own map[string]model.Binding) string {
	probe := model.Binding{Parameter: parameter, Value: "x"}
	if err := probe.Canonicalize(); err != nil {
		return ""
	}
	if _, ok := own[probe.ParameterKey]; ok {
		return probe.ParameterKey
	}
	return ""
}
