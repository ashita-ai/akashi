package server

import (
	"net/http"
	"time"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
)

// validatePairRequest is the JSON body for POST /v1/admin/conflicts/validate-pair.
type validatePairRequest struct {
	OutcomeA        string  `json:"outcome_a"`
	OutcomeB        string  `json:"outcome_b"`
	TypeA           string  `json:"type_a"`
	TypeB           string  `json:"type_b"`
	AgentA          string  `json:"agent_a"`
	AgentB          string  `json:"agent_b"`
	ReasoningA      string  `json:"reasoning_a,omitempty"`
	ReasoningB      string  `json:"reasoning_b,omitempty"`
	ProjectA        string  `json:"project_a,omitempty"`
	ProjectB        string  `json:"project_b,omitempty"`
	TopicSimilarity float64 `json:"topic_similarity,omitempty"`
}

type validatePairResponse struct {
	Relationship string `json:"relationship"`
	Category     string `json:"category"`
	Severity     string `json:"severity"`
	Explanation  string `json:"explanation"`
}

// HandleValidatePair handles POST /v1/admin/conflicts/validate-pair.
// Runs the configured LLM validator against a single decision pair.
// Returns 501 if no validator is configured.
func (h *Handlers) HandleValidatePair(w http.ResponseWriter, r *http.Request) {
	if h.conflictValidator == nil {
		writeError(w, r, http.StatusNotImplemented, model.ErrCodeNotImplemented,
			"no conflict validator configured (set AKASHI_CONFLICT_LLM_MODEL or OPENAI_API_KEY)")
		return
	}
	if _, ok := h.conflictValidator.(conflicts.NoopValidator); ok {
		writeError(w, r, http.StatusNotImplemented, model.ErrCodeNotImplemented,
			"conflict validator is noop (set AKASHI_CONFLICT_LLM_MODEL or OPENAI_API_KEY)")
		return
	}

	var req validatePairRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}
	if req.OutcomeA == "" || req.OutcomeB == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"outcome_a and outcome_b are required")
		return
	}

	input := conflicts.ValidateInput{
		OutcomeA:        req.OutcomeA,
		OutcomeB:        req.OutcomeB,
		TypeA:           req.TypeA,
		TypeB:           req.TypeB,
		AgentA:          req.AgentA,
		AgentB:          req.AgentB,
		CreatedA:        time.Now(),
		CreatedB:        time.Now(),
		ReasoningA:      req.ReasoningA,
		ReasoningB:      req.ReasoningB,
		ProjectA:        req.ProjectA,
		ProjectB:        req.ProjectB,
		TopicSimilarity: req.TopicSimilarity,
	}

	result, err := h.conflictValidator.Validate(r.Context(), input)
	if err != nil {
		h.writeInternalError(w, r, "conflict validation failed", err)
		return
	}

	writeJSON(w, r, http.StatusOK, validatePairResponse{
		Relationship: result.Relationship,
		Category:     result.Category,
		Severity:     result.Severity,
		Explanation:  result.Explanation,
	})
}

type conflictEvalResponse struct {
	Metrics conflicts.EvalMetrics  `json:"metrics"`
	Results []conflicts.EvalResult `json:"results"`
}

// HandleConflictEval handles POST /v1/admin/conflicts/eval.
// Runs the default evaluation dataset against the configured validator
// and returns precision/recall metrics.
func (h *Handlers) HandleConflictEval(w http.ResponseWriter, r *http.Request) {
	if h.conflictValidator == nil {
		writeError(w, r, http.StatusNotImplemented, model.ErrCodeNotImplemented,
			"no conflict validator configured")
		return
	}
	if _, ok := h.conflictValidator.(conflicts.NoopValidator); ok {
		writeError(w, r, http.StatusNotImplemented, model.ErrCodeNotImplemented,
			"conflict validator is noop — eval requires an LLM validator")
		return
	}

	dataset := conflicts.DefaultEvalDataset()
	results := make([]conflicts.EvalResult, 0, len(dataset))

	for _, pair := range dataset {
		result, err := h.conflictValidator.Validate(r.Context(), pair.Input)

		er := conflicts.EvalResult{
			Label:                pair.Label,
			ExpectedRelationship: pair.ExpectedRelationship,
		}

		if err != nil {
			er.Error = err.Error()
			results = append(results, er)
			continue
		}

		expectedIsConflict := pair.ExpectedRelationship == "contradiction" || pair.ExpectedRelationship == "supersession"
		actualIsConflict := result.IsConflict()

		er.ActualRelationship = result.Relationship
		er.Correct = result.Relationship == pair.ExpectedRelationship
		er.ConflictExpected = expectedIsConflict
		er.ConflictActual = actualIsConflict
		er.Explanation = result.Explanation
		results = append(results, er)
	}

	metrics := conflicts.ComputeMetrics(results)
	writeJSON(w, r, http.StatusOK, conflictEvalResponse{
		Metrics: metrics,
		Results: results,
	})
}
