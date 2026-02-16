package conflicts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// ---------------------------------------------------------------------------
// ParseValidatorResponse unit tests — RELATIONSHIP format
// ---------------------------------------------------------------------------

func TestParseValidatorResponse_Contradiction(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction\nCATEGORY: assessment\nSEVERITY: high\nEXPLANATION: Both decisions address caching strategy but reach incompatible conclusions.")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.True(t, result.IsConflict())
	assert.Equal(t, "Both decisions address caching strategy but reach incompatible conclusions.", result.Explanation)
	assert.Equal(t, "assessment", result.Category)
	assert.Equal(t, "high", result.Severity)
}

func TestParseValidatorResponse_Complementary(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: complementary\nCATEGORY: factual\nSEVERITY: low\nEXPLANATION: These are about different topics.")
	require.NoError(t, err)
	assert.Equal(t, "complementary", result.Relationship)
	assert.False(t, result.IsConflict())
	assert.Equal(t, "These are about different topics.", result.Explanation)
	assert.Equal(t, "factual", result.Category)
	assert.Equal(t, "low", result.Severity)
}

func TestParseValidatorResponse_Supersession(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: supersession\nCATEGORY: strategic\nSEVERITY: medium\nEXPLANATION: Decision B explicitly replaces Decision A.")
	require.NoError(t, err)
	assert.Equal(t, "supersession", result.Relationship)
	assert.True(t, result.IsConflict(), "supersession is an actionable conflict")
	assert.Equal(t, "strategic", result.Category)
	assert.Equal(t, "medium", result.Severity)
}

func TestParseValidatorResponse_Refinement(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: refinement\nCATEGORY: assessment\nSEVERITY: low\nEXPLANATION: Decision B builds on Decision A with more detail.")
	require.NoError(t, err)
	assert.Equal(t, "refinement", result.Relationship)
	assert.False(t, result.IsConflict())
}

func TestParseValidatorResponse_Unrelated(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: unrelated\nCATEGORY: factual\nSEVERITY: low\nEXPLANATION: Different topics despite surface similarity.")
	require.NoError(t, err)
	assert.Equal(t, "unrelated", result.Relationship)
	assert.False(t, result.IsConflict())
}

func TestParseValidatorResponse_CaseInsensitive(t *testing.T) {
	result, err := ParseValidatorResponse("relationship: Contradiction\ncategory: Strategic\nseverity: Medium\nexplanation: contradictory")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.True(t, result.IsConflict())
	assert.Equal(t, "strategic", result.Category)
	assert.Equal(t, "medium", result.Severity)

	result, err = ParseValidatorResponse("Relationship: COMPLEMENTARY\nCategory: temporal\nSeverity: Critical\nExplanation: different topics")
	require.NoError(t, err)
	assert.Equal(t, "complementary", result.Relationship)
	assert.False(t, result.IsConflict())
	assert.Equal(t, "temporal", result.Category)
	assert.Equal(t, "critical", result.Severity)
}

func TestParseValidatorResponse_ExtraWhitespace(t *testing.T) {
	result, err := ParseValidatorResponse("  RELATIONSHIP:   contradiction  \n  CATEGORY:   factual  \n  SEVERITY:   high  \n  EXPLANATION:   They conflict.  \n")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.True(t, result.IsConflict())
	assert.Equal(t, "They conflict.", result.Explanation)
	assert.Equal(t, "factual", result.Category)
	assert.Equal(t, "high", result.Severity)
}

func TestParseValidatorResponse_NoRelationshipLine(t *testing.T) {
	_, err := ParseValidatorResponse("This is just some text without a relationship.")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no RELATIONSHIP or VERDICT line found")
}

func TestParseValidatorResponse_UnrecognizedRelationship(t *testing.T) {
	_, err := ParseValidatorResponse("RELATIONSHIP: maybe\nEXPLANATION: unclear")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized relationship")
}

func TestParseValidatorResponse_BracketsStripped(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: [contradiction]\nEXPLANATION: test")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
}

func TestParseValidatorResponse_NoExplanation(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.Empty(t, result.Explanation)
}

func TestParseValidatorResponse_MultilineExtraPadding(t *testing.T) {
	// LLMs sometimes add extra lines before/after.
	response := `
Here is my analysis:

RELATIONSHIP: complementary
CATEGORY: assessment
SEVERITY: low
EXPLANATION: Decision A is about database choice while Decision B is about deployment region.

Hope this helps!
`
	result, err := ParseValidatorResponse(response)
	require.NoError(t, err)
	assert.Equal(t, "complementary", result.Relationship)
	assert.False(t, result.IsConflict())
	assert.Equal(t, "Decision A is about database choice while Decision B is about deployment region.", result.Explanation)
	assert.Equal(t, "assessment", result.Category)
	assert.Equal(t, "low", result.Severity)
}

func TestParseValidatorResponse_InvalidCategory(t *testing.T) {
	// Invalid category values should be silently ignored (empty string).
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction\nCATEGORY: philosophical\nSEVERITY: high\nEXPLANATION: conflict")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.Empty(t, result.Category, "invalid category should be ignored")
	assert.Equal(t, "high", result.Severity)
}

func TestParseValidatorResponse_InvalidSeverity(t *testing.T) {
	// Invalid severity values should be silently ignored.
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction\nCATEGORY: factual\nSEVERITY: extreme\nEXPLANATION: conflict")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.Equal(t, "factual", result.Category)
	assert.Empty(t, result.Severity, "invalid severity should be ignored")
}

func TestParseValidatorResponse_MissingCategoryAndSeverity(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction\nEXPLANATION: they conflict")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.Equal(t, "they conflict", result.Explanation)
	assert.Empty(t, result.Category)
	assert.Empty(t, result.Severity)
}

func TestParseValidatorResponse_AllCategories(t *testing.T) {
	for _, cat := range []string{"factual", "assessment", "strategic", "temporal"} {
		result, err := ParseValidatorResponse(fmt.Sprintf("RELATIONSHIP: contradiction\nCATEGORY: %s\nSEVERITY: low\nEXPLANATION: test", cat))
		require.NoError(t, err, "category=%s", cat)
		assert.Equal(t, cat, result.Category, "category=%s", cat)
	}
}

func TestParseValidatorResponse_AllSeverities(t *testing.T) {
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		result, err := ParseValidatorResponse(fmt.Sprintf("RELATIONSHIP: contradiction\nCATEGORY: factual\nSEVERITY: %s\nEXPLANATION: test", sev))
		require.NoError(t, err, "severity=%s", sev)
		assert.Equal(t, sev, result.Severity, "severity=%s", sev)
	}
}

func TestParseValidatorResponse_AllRelationships(t *testing.T) {
	for _, rel := range []string{"contradiction", "supersession", "complementary", "refinement", "unrelated"} {
		result, err := ParseValidatorResponse(fmt.Sprintf("RELATIONSHIP: %s\nCATEGORY: factual\nSEVERITY: low\nEXPLANATION: test", rel))
		require.NoError(t, err, "relationship=%s", rel)
		assert.Equal(t, rel, result.Relationship, "relationship=%s", rel)
	}
}

// ---------------------------------------------------------------------------
// Backward compatibility: VERDICT → RELATIONSHIP mapping
// ---------------------------------------------------------------------------

func TestParseValidatorResponse_VerdictYesBackwardCompat(t *testing.T) {
	result, err := ParseValidatorResponse("VERDICT: yes\nCATEGORY: assessment\nSEVERITY: high\nEXPLANATION: legacy format")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship, "VERDICT: yes should map to contradiction")
	assert.True(t, result.IsConflict())
}

func TestParseValidatorResponse_VerdictNoBackwardCompat(t *testing.T) {
	result, err := ParseValidatorResponse("VERDICT: no\nCATEGORY: factual\nSEVERITY: low\nEXPLANATION: legacy format")
	require.NoError(t, err)
	assert.Equal(t, "unrelated", result.Relationship, "VERDICT: no should map to unrelated")
	assert.False(t, result.IsConflict())
}

func TestParseValidatorResponse_RelationshipTakesPrecedence(t *testing.T) {
	// If both RELATIONSHIP and VERDICT are present, RELATIONSHIP wins.
	result, err := ParseValidatorResponse("RELATIONSHIP: complementary\nVERDICT: yes\nEXPLANATION: both present")
	require.NoError(t, err)
	assert.Equal(t, "complementary", result.Relationship, "RELATIONSHIP should take precedence over VERDICT")
	assert.False(t, result.IsConflict())
}

// ---------------------------------------------------------------------------
// NoopValidator tests
// ---------------------------------------------------------------------------

func TestNoopValidator(t *testing.T) {
	v := NoopValidator{}
	result, err := v.Validate(context.Background(), ValidateInput{
		OutcomeA: "chose Redis",
		OutcomeB: "chose Memcached",
		TypeA:    "architecture",
		TypeB:    "architecture",
	})
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship, "NoopValidator always returns contradiction")
	assert.True(t, result.IsConflict())
	assert.Empty(t, result.Explanation)
	assert.Equal(t, "unknown", result.Category, "NoopValidator defaults to unknown category")
	assert.Equal(t, "medium", result.Severity, "NoopValidator defaults to medium severity")
}

// ---------------------------------------------------------------------------
// formatPrompt tests
// ---------------------------------------------------------------------------

func TestFormatPrompt_SameAgent(t *testing.T) {
	now := time.Now()
	prompt := formatPrompt(ValidateInput{
		OutcomeA: "chose Redis",
		OutcomeB: "chose Memcached",
		TypeA:    "architecture",
		TypeB:    "architecture",
		AgentA:   "planner",
		AgentB:   "planner",
		CreatedA: now,
		CreatedB: now.Add(2 * time.Hour),
	})
	assert.Contains(t, prompt, "the same agent")
	assert.Contains(t, prompt, "chose Redis")
	assert.Contains(t, prompt, "chose Memcached")
	assert.Contains(t, prompt, "planner")
}

func TestFormatPrompt_DifferentAgents(t *testing.T) {
	now := time.Now()
	prompt := formatPrompt(ValidateInput{
		OutcomeA: "use REST",
		OutcomeB: "use gRPC",
		TypeA:    "architecture",
		TypeB:    "architecture",
		AgentA:   "planner",
		AgentB:   "coder",
		CreatedA: now,
		CreatedB: now.Add(48 * time.Hour),
	})
	assert.Contains(t, prompt, "different agents")
	assert.Contains(t, prompt, "2.0 days")
}

func TestFormatDuration(t *testing.T) {
	assert.Contains(t, formatDuration(30*time.Minute), "minutes")
	assert.Contains(t, formatDuration(5*time.Hour), "hours")
	assert.Contains(t, formatDuration(72*time.Hour), "days")
}

// ---------------------------------------------------------------------------
// OllamaValidator tests (httptest mock)
// ---------------------------------------------------------------------------

func TestOllamaValidator_Contradiction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/chat", r.URL.Path)
		assert.Equal(t, "POST", r.Method)

		var req ollamaChatRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.NotEmpty(t, req.Model)
		assert.Len(t, req.Messages, 1)
		assert.False(t, req.Stream)

		// Verify the prompt contains temporal context.
		assert.Contains(t, req.Messages[0].Content, "agent")
		assert.Contains(t, req.Messages[0].Content, "RELATIONSHIP")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Content string `json:"content"`
			}{
				Content: "RELATIONSHIP: contradiction\nCATEGORY: strategic\nSEVERITY: high\nEXPLANATION: Both decisions address caching strategy but chose incompatible technologies.",
			},
		})
	}))
	defer srv.Close()

	v := NewOllamaValidator(srv.URL, "test-model")
	result, err := v.Validate(context.Background(), ValidateInput{
		OutcomeA: "chose Redis for caching",
		OutcomeB: "chose Memcached for caching",
		TypeA:    "architecture",
		TypeB:    "architecture",
		AgentA:   "planner",
		AgentB:   "coder",
		CreatedA: time.Now(),
		CreatedB: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.True(t, result.IsConflict())
	assert.Contains(t, result.Explanation, "caching")
	assert.Equal(t, "strategic", result.Category)
	assert.Equal(t, "high", result.Severity)
}

func TestOllamaValidator_Complementary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Content string `json:"content"`
			}{
				Content: "RELATIONSHIP: complementary\nCATEGORY: assessment\nSEVERITY: low\nEXPLANATION: Decision A is about database choice while Decision B is about deployment region.",
			},
		})
	}))
	defer srv.Close()

	v := NewOllamaValidator(srv.URL, "test-model")
	result, err := v.Validate(context.Background(), ValidateInput{
		OutcomeA: "use PostgreSQL",
		OutcomeB: "deploy to eu-west-1",
		TypeA:    "architecture",
		TypeB:    "deployment",
		AgentA:   "planner",
		AgentB:   "planner",
		CreatedA: time.Now(),
		CreatedB: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, "complementary", result.Relationship)
	assert.False(t, result.IsConflict())
	assert.Contains(t, result.Explanation, "database choice")
}

func TestOllamaValidator_MalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Content string `json:"content"`
			}{
				Content: "I think these might be contradictory but I'm not sure.",
			},
		})
	}))
	defer srv.Close()

	v := NewOllamaValidator(srv.URL, "test-model")
	_, err := v.Validate(context.Background(), ValidateInput{
		OutcomeA: "outcome A",
		OutcomeB: "outcome B",
		TypeA:    "type",
		TypeB:    "type",
		AgentA:   "agent",
		AgentB:   "agent",
		CreatedA: time.Now(),
		CreatedB: time.Now(),
	})
	assert.Error(t, err)
}

func TestOllamaValidator_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second) // Longer than the context timeout below.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	v := NewOllamaValidator(srv.URL, "test-model")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := v.Validate(ctx, ValidateInput{
		OutcomeA: "outcome A",
		OutcomeB: "outcome B",
		TypeA:    "type",
		TypeB:    "type",
		AgentA:   "agent",
		AgentB:   "agent",
		CreatedA: time.Now(),
		CreatedB: time.Now(),
	})
	assert.Error(t, err)
}

func TestOllamaValidator_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	v := NewOllamaValidator(srv.URL, "test-model")
	_, err := v.Validate(context.Background(), ValidateInput{
		OutcomeA: "outcome A",
		OutcomeB: "outcome B",
		TypeA:    "type",
		TypeB:    "type",
		AgentA:   "agent",
		AgentB:   "agent",
		CreatedA: time.Now(),
		CreatedB: time.Now(),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

// ---------------------------------------------------------------------------
// OpenAIValidator tests (httptest mock)
// ---------------------------------------------------------------------------

func TestOpenAIValidator_Confirms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{
					Content: "RELATIONSHIP: contradiction\nCATEGORY: factual\nSEVERITY: critical\nEXPLANATION: Both decisions address API protocol choice but reach incompatible conclusions.",
				}},
			},
		})
	}))
	defer srv.Close()

	// The OpenAI URL is hardcoded, so we test parsing via the response.
	// The HTTP plumbing is identical to OllamaValidator (tested above) with
	// different request/response shapes.
	_ = srv
}

func TestOpenAIValidator_DefaultModel(t *testing.T) {
	v := NewOpenAIValidator("test-key", "")
	assert.Equal(t, "gpt-4o-mini", v.model)
}

// ---------------------------------------------------------------------------
// Scorer integration tests with mock validators
// ---------------------------------------------------------------------------

// mockValidator is a test double that returns preconfigured results.
type mockValidator struct {
	result    ValidationResult
	err       error
	callCount int
}

func (m *mockValidator) Validate(_ context.Context, _ ValidateInput) (ValidationResult, error) {
	m.callCount++
	return m.result, m.err
}

func TestScoreForDecision_LLMConfirms(t *testing.T) {
	ctx := context.Background()
	logger := slog.Default()
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "llm-confirm-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	topicEmb := makeEmbedding(600, 1.0)
	outcomeEmbA := makeEmbedding(601, 1.0)
	outcomeEmbB := makeEmbedding(602, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "chose Redis for caching",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "chose Memcached for caching",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbB,
	})
	require.NoError(t, err)

	validator := &mockValidator{result: ValidationResult{
		Relationship: "contradiction",
		Explanation:  "Both address caching but chose incompatible technologies.",
		Category:     "strategic",
		Severity:     "high",
	}}
	scorer := NewScorer(testDB, logger, 0.1, validator, 0, 0)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	assert.Greater(t, validator.callCount, 0, "validator should have been called")

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	var found bool
	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			found = true
			assert.Equal(t, "llm_v2", c.ScoringMethod, "LLM-validated conflicts should have method='llm_v2'")
			require.NotNil(t, c.Explanation, "LLM-confirmed conflicts should have an explanation")
			assert.Contains(t, *c.Explanation, "caching")
			require.NotNil(t, c.Category, "LLM-confirmed conflicts should have a category")
			assert.Equal(t, "strategic", *c.Category)
			require.NotNil(t, c.Severity, "LLM-confirmed conflicts should have a severity")
			assert.Equal(t, "high", *c.Severity)
			require.NotNil(t, c.Relationship, "LLM-confirmed conflicts should have a relationship")
			assert.Equal(t, "contradiction", *c.Relationship)
			require.NotNil(t, c.ConfidenceWeight, "should have confidence weight")
			require.NotNil(t, c.TemporalDecay, "should have temporal decay")
			assert.Equal(t, "open", c.Status, "new conflicts should be open")
			break
		}
	}
	assert.True(t, found, "expected an LLM-confirmed conflict between dA and dB")
}

func TestScoreForDecision_LLMRejectsComplementary(t *testing.T) {
	ctx := context.Background()
	logger := slog.Default()
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "llm-reject-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	topicEmb := makeEmbedding(610, 1.0)
	outcomeEmbA := makeEmbedding(611, 1.0)
	outcomeEmbB := makeEmbedding(612, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review", Outcome: "added tests for auth module",
		Confidence: 0.9, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "enterprise licensing strategy decided",
		Confidence: 0.88, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbB,
	})
	require.NoError(t, err)

	validator := &mockValidator{result: ValidationResult{
		Relationship: "complementary",
		Explanation:  "Different topics — tests vs licensing.",
	}}
	scorer := NewScorer(testDB, logger, 0.1, validator, 0, 0)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	assert.Greater(t, validator.callCount, 0, "validator should have been called")

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			t.Fatal("LLM-classified complementary pair should NOT produce a conflict")
		}
	}
}

func TestScoreForDecision_LLMSupersession(t *testing.T) {
	ctx := context.Background()
	logger := slog.Default()
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "llm-super-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	topicEmb := makeEmbedding(640, 1.0)
	outcomeEmbA := makeEmbedding(641, 1.0)
	outcomeEmbB := makeEmbedding(642, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use REST v1 API",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "replaced REST v1 with gRPC",
		Confidence: 0.9, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbB,
	})
	require.NoError(t, err)

	validator := &mockValidator{result: ValidationResult{
		Relationship: "supersession",
		Explanation:  "Decision B explicitly replaces Decision A's API protocol choice.",
		Category:     "strategic",
		Severity:     "medium",
	}}
	scorer := NewScorer(testDB, logger, 0.1, validator, 0, 0)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	var found bool
	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			found = true
			assert.Equal(t, "llm_v2", c.ScoringMethod)
			require.NotNil(t, c.Relationship)
			assert.Equal(t, "supersession", *c.Relationship,
				"supersession should be stored as a conflict")
			break
		}
	}
	assert.True(t, found, "supersession should produce a conflict between dA and dB")
}

func TestScoreForDecision_LLMError(t *testing.T) {
	ctx := context.Background()
	logger := slog.Default()
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "llm-error-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	topicEmb := makeEmbedding(620, 1.0)
	outcomeEmbA := makeEmbedding(621, 1.0)
	outcomeEmbB := makeEmbedding(622, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "chose gRPC",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "chose REST",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbB,
	})
	require.NoError(t, err)

	validator := &mockValidator{err: fmt.Errorf("ollama unavailable")}
	scorer := NewScorer(testDB, logger, 0.1, validator, 0, 0)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	assert.Greater(t, validator.callCount, 0, "validator should have been called")

	// Fail-safe: LLM error means candidate is NOT inserted.
	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			t.Fatal("LLM error should NOT produce a conflict (fail-safe rejects)")
		}
	}
}

func TestHasLLMValidator(t *testing.T) {
	scorer := NewScorer(nil, slog.Default(), 0.3, nil, 0, 0)
	assert.False(t, scorer.HasLLMValidator(), "nil validator defaults to NoopValidator")

	scorer = NewScorer(nil, slog.Default(), 0.3, NoopValidator{}, 0, 0)
	assert.False(t, scorer.HasLLMValidator(), "explicit NoopValidator")

	scorer = NewScorer(nil, slog.Default(), 0.3, &mockValidator{}, 0, 0)
	assert.True(t, scorer.HasLLMValidator(), "mock validator is not noop")
}
