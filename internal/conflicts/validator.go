package conflicts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ValidateInput holds all context needed for relationship classification.
type ValidateInput struct {
	OutcomeA string
	OutcomeB string
	TypeA    string
	TypeB    string
	AgentA   string
	AgentB   string
	CreatedA time.Time
	CreatedB time.Time

	// Enrichment fields — may be empty when context is unavailable.
	ReasoningA   string // Decision reasoning.
	ReasoningB   string
	RepoA        string // From agent_context["repo"].
	RepoB        string
	TaskA        string // From agent_context["task"].
	TaskB        string
	SessionIDA   string // UUID string.
	SessionIDB   string
	FullOutcomeA string // Full outcome when OutcomeA is a claim fragment.
	FullOutcomeB string
}

// ValidationResult holds the structured output from an LLM validation call.
type ValidationResult struct {
	Relationship string // contradiction, supersession, complementary, refinement, unrelated
	Explanation  string
	Category     string // factual, assessment, strategic, temporal
	Severity     string // critical, high, medium, low
}

// IsConflict returns true if the relationship represents an actionable conflict.
func (r ValidationResult) IsConflict() bool {
	return r.Relationship == "contradiction" || r.Relationship == "supersession"
}

// Validator classifies the relationship between two decision outcomes.
// The embedding scorer finds candidates (cheap, fast); the validator classifies
// them (precise, slower). This two-stage design keeps false positives low
// without requiring an LLM call for every decision pair.
type Validator interface {
	Validate(ctx context.Context, input ValidateInput) (ValidationResult, error)
}

// validCategories and validSeverities define the allowed values for classification.
var validCategories = map[string]bool{"factual": true, "assessment": true, "strategic": true, "temporal": true}
var validSeverities = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}

// validRelationships defines the allowed values for relationship classification.
var validRelationships = map[string]bool{
	"contradiction": true,
	"supersession":  true,
	"complementary": true,
	"refinement":    true,
	"unrelated":     true,
}

// formatPrompt builds the validation prompt with temporal, agent, project, and
// session context. The prompt is constructed dynamically to include only the
// context signals that are available, avoiding noise from empty fields.
func formatPrompt(input ValidateInput) string {
	timeDelta := input.CreatedB.Sub(input.CreatedA).Abs()
	deltaStr := formatDuration(timeDelta)

	agentContext := "the same agent"
	if input.AgentA != input.AgentB {
		agentContext = "different agents"
	}

	var b strings.Builder
	b.WriteString("You are a relationship classifier for an AI decision audit system.\n\n")

	// --- Decision A ---
	fmt.Fprintf(&b, "Decision A (%s, by agent %q, recorded %s):\n%s\n",
		input.TypeA, input.AgentA, input.CreatedA.Format(time.RFC3339), input.OutcomeA)
	if input.FullOutcomeA != "" && input.FullOutcomeA != input.OutcomeA {
		fmt.Fprintf(&b, "[Full decision context: %s]\n", truncateRunes(input.FullOutcomeA, 500))
	}
	if input.ReasoningA != "" {
		fmt.Fprintf(&b, "[Reasoning: %s]\n", truncateRunes(input.ReasoningA, 300))
	}

	// --- Decision B ---
	fmt.Fprintf(&b, "\nDecision B (%s, by agent %q, recorded %s):\n%s\n",
		input.TypeB, input.AgentB, input.CreatedB.Format(time.RFC3339), input.OutcomeB)
	if input.FullOutcomeB != "" && input.FullOutcomeB != input.OutcomeB {
		fmt.Fprintf(&b, "[Full decision context: %s]\n", truncateRunes(input.FullOutcomeB, 500))
	}
	if input.ReasoningB != "" {
		fmt.Fprintf(&b, "[Reasoning: %s]\n", truncateRunes(input.ReasoningB, 300))
	}

	// --- Temporal and agent context ---
	fmt.Fprintf(&b, "\nContext: These decisions were recorded %s apart by %s.\n", deltaStr, agentContext)

	// --- Project context (#168: cross-project confusion) ---
	if input.RepoA != "" && input.RepoB != "" {
		if input.RepoA != input.RepoB {
			fmt.Fprintf(&b, "DIFFERENT PROJECTS: Decision A is about %q, Decision B is about %q. Decisions about different codebases are almost always UNRELATED.\n",
				input.RepoA, input.RepoB)
		} else {
			fmt.Fprintf(&b, "Same project: %s\n", input.RepoA)
		}
	}
	if input.TaskA != "" {
		fmt.Fprintf(&b, "Task A: %s\n", truncateRunes(input.TaskA, 100))
	}
	if input.TaskB != "" {
		fmt.Fprintf(&b, "Task B: %s\n", truncateRunes(input.TaskB, 100))
	}

	// --- Session context (#170: temporal refinement) ---
	if input.SessionIDA != "" && input.SessionIDB != "" && input.SessionIDA == input.SessionIDB {
		b.WriteString("SAME SESSION: Both decisions were recorded in the same work session. Sequential decisions are typically REFINEMENT or COMPLEMENTARY, not contradictions.\n")
	}

	// --- Classification instructions ---
	b.WriteString(`
Classify the RELATIONSHIP between these two decisions:

- CONTRADICTION: Incompatible positions on the same specific question. Cannot both be true.
- SUPERSESSION: One decision explicitly replaces or reverses the other.
- COMPLEMENTARY: Different findings about different aspects. Both can be true simultaneously.
- REFINEMENT: One decision deepens or builds on the other.
- UNRELATED: Different topics despite surface similarity.

IMPORTANT for assessments and code reviews:
- A review that reports finding bugs does NOT contradict those bug reports — it discovered them.
- A summary assessment ("security is strong") and a detailed review ("found vulnerability X") are NOT contradictions. Detailed reviews always find issues that summaries don't mention.
- Two reviews finding different issues in the same codebase are complementary, not contradictory.
- For assessments to contradict, they must make OPPOSITE claims about the SAME specific finding.

RELATIONSHIP: one of [contradiction, supersession, complementary, refinement, unrelated]
CATEGORY: factual, assessment, strategic, or temporal
SEVERITY: critical, high, medium, or low
EXPLANATION: one sentence`)

	return b.String()
}

// truncateRunes truncates a string to maxLen runes, appending "..." if truncated.
// Rune-safe to avoid splitting multi-byte characters.
func truncateRunes(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// formatDuration produces a human-readable duration string.
func formatDuration(d time.Duration) string {
	hours := d.Hours()
	switch {
	case hours < 1:
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	case hours < 24:
		return fmt.Sprintf("%.1f hours", hours)
	default:
		return fmt.Sprintf("%.1f days", hours/24)
	}
}

// ParseValidatorResponse extracts the relationship, category, severity, and
// explanation from an LLM response. If parsing fails, returns an error to
// enforce fail-safe behavior: ambiguous responses are treated as rejections.
func ParseValidatorResponse(response string) (ValidationResult, error) {
	lines := strings.Split(strings.TrimSpace(response), "\n")

	var relationship, explanation, category, severity string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "relationship:"):
			relationship = strings.ToLower(strings.TrimSpace(trimmed[len("relationship:"):]))
		case strings.HasPrefix(lower, "verdict:"):
			// Backward compatibility: map old-style yes/no to relationship.
			verdict := strings.ToLower(strings.TrimSpace(trimmed[len("verdict:"):]))
			if relationship == "" {
				switch verdict {
				case "yes":
					relationship = "contradiction"
				case "no":
					relationship = "unrelated"
				}
			}
		case strings.HasPrefix(lower, "explanation:"):
			explanation = strings.TrimSpace(trimmed[len("explanation:"):])
		case strings.HasPrefix(lower, "category:"):
			category = strings.ToLower(strings.TrimSpace(trimmed[len("category:"):]))
		case strings.HasPrefix(lower, "severity:"):
			severity = strings.ToLower(strings.TrimSpace(trimmed[len("severity:"):]))
		}
	}

	if relationship == "" {
		return ValidationResult{}, fmt.Errorf("validator: no RELATIONSHIP or VERDICT line found in response")
	}

	// Normalize: strip any brackets or extra text (e.g. "[contradiction]" → "contradiction").
	relationship = strings.Trim(relationship, "[] ")

	if !validRelationships[relationship] {
		return ValidationResult{}, fmt.Errorf("validator: unrecognized relationship %q", relationship)
	}

	// Normalize category and severity — ignore invalid values rather than failing.
	if !validCategories[category] {
		category = ""
	}
	if !validSeverities[severity] {
		severity = ""
	}

	return ValidationResult{
		Relationship: relationship,
		Explanation:  explanation,
		Category:     category,
		Severity:     severity,
	}, nil
}

// NoopValidator always returns a contradiction result. This preserves the
// current behavior when no LLM is configured: embedding-scored candidates
// are inserted without validation. Users who want precision must configure
// an LLM model.
type NoopValidator struct{}

func (NoopValidator) Validate(_ context.Context, _ ValidateInput) (ValidationResult, error) {
	return ValidationResult{
		Relationship: "contradiction",
		Category:     "unknown",
		Severity:     "medium",
	}, nil
}

// perCallTimeout is the maximum time for a single LLM validation call.
// Separate from the scorer's overall context timeout so one slow call
// doesn't block the entire scoring pass.
const perCallTimeout = 15 * time.Second

// OllamaValidator validates conflict candidates using a local Ollama chat model.
// Reuses the existing OLLAMA_URL configuration. The model should be a text
// generation model (e.g., qwen2.5:3b), not an embedding model.
type OllamaValidator struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewOllamaValidator creates a validator that calls Ollama's chat API.
func NewOllamaValidator(baseURL, model string) *OllamaValidator {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaValidator{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: perCallTimeout + 5*time.Second, // HTTP timeout slightly beyond per-call context timeout.
		},
	}
}

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
}

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

func (v *OllamaValidator) Validate(ctx context.Context, input ValidateInput) (ValidationResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	prompt := formatPrompt(input)

	body, err := json.Marshal(ollamaChatRequest{
		Model: v.model,
		Messages: []ollamaChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	})
	if err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, v.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ValidationResult{}, fmt.Errorf("ollama validator: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ValidationResult{}, fmt.Errorf("ollama validator: decode response: %w", err)
	}

	return ParseValidatorResponse(result.Message.Content)
}

// OpenAIValidator validates conflict candidates using the OpenAI chat API.
// Uses gpt-4o-mini for cost efficiency. Reuses the existing OPENAI_API_KEY.
type OpenAIValidator struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewOpenAIValidator creates a validator that calls the OpenAI chat completions API.
func NewOpenAIValidator(apiKey, model string) *OpenAIValidator {
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &OpenAIValidator{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: perCallTimeout + 5*time.Second,
		},
	}
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (v *OpenAIValidator) Validate(ctx context.Context, input ValidateInput) (ValidationResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	prompt := formatPrompt(input)

	body, err := json.Marshal(openAIChatRequest{
		Model: v.model,
		Messages: []openAIChatMessage{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.apiKey)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ValidationResult{}, fmt.Errorf("openai validator: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ValidationResult{}, fmt.Errorf("openai validator: decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return ValidationResult{}, fmt.Errorf("openai validator: no choices in response")
	}

	return ParseValidatorResponse(result.Choices[0].Message.Content)
}
