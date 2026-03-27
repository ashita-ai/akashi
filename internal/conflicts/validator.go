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
	ReasoningA        string // decision reasoning
	ReasoningB        string
	ProjectA          string // from agent_context["project"] (or legacy "repo")
	ProjectB          string
	TaskA             string // from agent_context["task"]
	TaskB             string
	SessionIDA        string // UUID string
	SessionIDB        string
	FullOutcomeA      string // full outcome when OutcomeA is a claim fragment
	FullOutcomeB      string
	TopicSimilarity   float64 // decision-level embedding similarity (0–1); 0 means unavailable
	PrecedentLinked   bool    // true when one decision cites the other via precedent_ref
	OutcomeSimilarity float64 // outcome embedding similarity (0–1); 0 means unavailable
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

	// --- Decision by agent A ---
	fmt.Fprintf(&b, "Agent %q decision (%s, recorded %s):\n%s\n",
		input.AgentA, input.TypeA, input.CreatedA.Format(time.RFC3339), input.OutcomeA)
	if input.FullOutcomeA != "" && input.FullOutcomeA != input.OutcomeA {
		fmt.Fprintf(&b, "[Full decision context: %s]\n", truncateRunes(input.FullOutcomeA, 500))
	}
	if input.ReasoningA != "" {
		fmt.Fprintf(&b, "[Reasoning: %s]\n", truncateRunes(input.ReasoningA, 300))
	}

	// --- Decision by agent B ---
	fmt.Fprintf(&b, "\nAgent %q decision (%s, recorded %s):\n%s\n",
		input.AgentB, input.TypeB, input.CreatedB.Format(time.RFC3339), input.OutcomeB)
	if input.FullOutcomeB != "" && input.FullOutcomeB != input.OutcomeB {
		fmt.Fprintf(&b, "[Full decision context: %s]\n", truncateRunes(input.FullOutcomeB, 500))
	}
	if input.ReasoningB != "" {
		fmt.Fprintf(&b, "[Reasoning: %s]\n", truncateRunes(input.ReasoningB, 300))
	}

	// --- Temporal and agent context ---
	fmt.Fprintf(&b, "\nContext: These decisions were recorded %s apart by %s.\n", deltaStr, agentContext)

	// --- Project context (#168: cross-project confusion) ---
	if input.ProjectA != "" && input.ProjectB != "" {
		if input.ProjectA != input.ProjectB {
			fmt.Fprintf(&b, "DIFFERENT PROJECTS: %q's decision is about %q, %q's decision is about %q. Decisions about different codebases are almost always UNRELATED.\n",
				input.AgentA, input.ProjectA, input.AgentB, input.ProjectB)
		} else {
			fmt.Fprintf(&b, "Same project: %s\n", input.ProjectA)
		}
	} else if input.AgentA != input.AgentB {
		// Repository names unavailable — guide the LLM to identify projects from outcome text.
		// Different agents frequently work on different codebases and use similar assessment
		// vocabulary (e.g. "comprehensive review", "aggregate score") without those reviews
		// being related. Cross-project confusion is the leading source of false positives.
		b.WriteString("PROJECT CONTEXT: Repository names are not recorded for these decisions. " +
			"Read the outcome text carefully for named codebases, products, or projects (e.g. proper nouns like product names, repository names, service names). " +
			"If the two decisions clearly refer to DIFFERENT named systems, classify as UNRELATED — different codebases cannot contradict each other. " +
			"Only classify as CONTRADICTION if both decisions are clearly about the SAME system and make incompatible claims about it.\n")
	}
	if input.TaskA != "" {
		fmt.Fprintf(&b, "Task (%s): %s\n", input.AgentA, truncateRunes(input.TaskA, 100))
	}
	if input.TaskB != "" {
		fmt.Fprintf(&b, "Task (%s): %s\n", input.AgentB, truncateRunes(input.TaskB, 100))
	}

	// --- Session context (#170: temporal refinement) ---
	if input.SessionIDA != "" && input.SessionIDB != "" && input.SessionIDA == input.SessionIDB {
		b.WriteString("SAME SESSION: Both decisions were recorded in the same work session. Sequential decisions are typically REFINEMENT or COMPLEMENTARY, not contradictions.\n")
	}

	// --- Topic similarity signal ---
	// When embedding similarity is high and agents differ, flag it explicitly.
	// Bi-encoders place same-topic decisions close together regardless of stance,
	// so high similarity here means "same domain" — not "same conclusion".
	if input.TopicSimilarity >= 0.70 && input.AgentA != input.AgentB {
		fmt.Fprintf(&b, "HIGH TOPIC OVERLAP: Embeddings show %.0f%% topic similarity, meaning both decisions address the same domain. Check whether the agents take OPPOSITE STANCES on the same specific question.\n",
			input.TopicSimilarity*100)
	}

	// --- Outcome similarity signal ---
	// When outcome embeddings are highly similar, the decisions likely agree.
	// This is distinct from topic similarity (which measures domain overlap):
	// high outcome similarity means the conclusions themselves are close.
	if input.OutcomeSimilarity >= 0.80 {
		fmt.Fprintf(&b, "HIGH OUTCOME SIMILARITY: Outcome embeddings show %.0f%% similarity. "+
			"When outcomes are this similar, the decisions likely AGREE on their conclusions "+
			"even if they address different aspects or layers. "+
			"Classify as COMPLEMENTARY unless they make genuinely incompatible claims.\n",
			input.OutcomeSimilarity*100)
	}

	// --- Precedent chain supersession hint (#452) ---
	// When one decision explicitly cites the other via precedent_ref and
	// contains supersession keywords, the heuristic filter passed it through
	// for LLM validation. Hint the LLM that this is a linked pair where the
	// later decision may reverse the earlier one.
	if input.PrecedentLinked {
		b.WriteString("PRECEDENT LINK: One decision explicitly cites the other as its precedent. " +
			"The later decision may REFINE the earlier one (building on it) or SUPERSEDE it (reversing the choice). " +
			"Pay close attention to whether the later decision's outcome contradicts or reverses the earlier decision's conclusion. " +
			"If it does, classify as SUPERSESSION.\n")
	}

	// --- Decision type workflow hint ---
	// When decision types suggest a sequential workflow (e.g., a review followed
	// by a fix), inject a strong hint to reduce false positives from the LLM
	// misclassifying cause-and-effect pairs as contradictions.
	if isWorkflowPair(input.TypeA, input.TypeB) {
		fmt.Fprintf(&b, "WORKFLOW PATTERN: Decision types %q → %q suggest a sequential workflow (analysis/review followed by implementation/fix). "+
			"These are almost always REFINEMENT or COMPLEMENTARY — one decision identifies issues, the other resolves them. "+
			"Only classify as CONTRADICTION if they make genuinely incompatible claims about the same question.\n",
			input.TypeA, input.TypeB)
	}

	// --- Classification instructions ---
	b.WriteString(`
Classify the RELATIONSHIP between these two decisions:

- CONTRADICTION: Incompatible positions on the same specific question. Cannot both be true simultaneously. Implementing one would require rejecting the other.
- SUPERSESSION: One decision explicitly replaces or reverses the other.
- COMPLEMENTARY: Different findings about different aspects. Both can be true simultaneously.
- REFINEMENT: One decision deepens or builds on the other without contradicting it.
- UNRELATED: Different topics despite surface similarity.

IMPORTANT for architecture and planning decisions:
- Two agents recommending DIFFERENT approaches to the SAME design question ARE contradictions. Example: "use nested structure X" vs "use flat structure Y for the same purpose" = CONTRADICTION.
- Ask: can both be implemented simultaneously? If yes → COMPLEMENTARY or REFINEMENT. If no → CONTRADICTION.
- An agent reversing its own prior choice is SUPERSESSION. A different agent disagreeing is CONTRADICTION.

IMPORTANT for assessments and code reviews:
- A review that reports finding bugs does NOT contradict those bug reports — it discovered them.
- A summary assessment ("security is strong") and a detailed review ("found vulnerability X") are NOT contradictions. Detailed reviews always find issues that summaries don't mention.
- Two reviews finding different issues in the same codebase are complementary, not contradictory.
- Two reviews of DIFFERENT codebases or products are UNRELATED — they cannot contradict each other.
- For assessments to contradict, they must make OPPOSITE claims about the SAME specific finding in the SAME system.
- A review at one scope (e.g. a single PR) finding "no issues" does NOT contradict a broader review (e.g. full codebase) finding issues. They examined different code.

IMPORTANT for sequential workflow (identify → fix):
- When one decision identifies a problem and a later decision implements a fix or improvement for that problem, classify as REFINEMENT — the fix builds on the finding.
- "X is naive/broken/missing" followed by "implemented improvements to X" is REFINEMENT, not CONTRADICTION. The second decision addresses the first.
- A code review finding issues followed by a bug fix resolving those issues is REFINEMENT or COMPLEMENTARY — never CONTRADICTION.

IMPORTANT for agreement detection:
- If both decisions recommend the SAME approach, technology, or tool — even with different framing or emphasis — they AGREE. Classify as COMPLEMENTARY.
- "Use X for purpose A" and "Use X for purpose B" is COMPLEMENTARY (both chose X), not CONTRADICTION.

RELATIONSHIP: one of [contradiction, supersession, complementary, refinement, unrelated]
CATEGORY: factual, assessment, strategic, or temporal
SEVERITY: critical, high, medium, or low
EXPLANATION: one sentence using agent names (not "Decision A" or "Decision B")`)

	return b.String()
}

// workflowPairs maps decision type pairs that represent sequential workflows
// (analysis/review followed by implementation/fix). When both types in a pair
// appear, the decisions are almost certainly cause-and-effect rather than
// contradictions. The key is the "earlier" type, the values are the "later" types.
var workflowPairs = map[string][]string{
	"code_review": {"bug_fix", "fix", "implementation", "refactor"},
	"assessment":  {"bug_fix", "fix", "implementation", "refactor", "architecture"},
	"review":      {"bug_fix", "fix", "implementation", "refactor"},
	"analysis":    {"bug_fix", "fix", "implementation", "architecture"},
	"audit":       {"bug_fix", "fix", "implementation", "refactor"},
}

// isWorkflowPair returns true if the two decision types suggest a sequential
// workflow (e.g., code_review → bug_fix) in either direction. This is used
// to inject a strong hint into the LLM prompt to reduce false positives.
func isWorkflowPair(typeA, typeB string) bool {
	a := strings.ToLower(typeA)
	b := strings.ToLower(typeB)
	if followers, ok := workflowPairs[a]; ok {
		for _, f := range followers {
			if b == f {
				return true
			}
		}
	}
	if followers, ok := workflowPairs[b]; ok {
		for _, f := range followers {
			if a == f {
				return true
			}
		}
	}
	return false
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
		// Strip leading markdown bold/italic markers that some LLMs add.
		// e.g. "**RELATIONSHIP:** CONTRADICTION" → "RELATIONSHIP:** CONTRADICTION"
		trimmed = strings.TrimLeft(trimmed, "*_")
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "relationship:"):
			// Trim markdown markers that can appear between ":" and the value.
			relationship = strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("relationship:"):]), "*_ "))
		case strings.HasPrefix(lower, "verdict:"):
			// Backward compatibility: map old-style yes/no to relationship.
			verdict := strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("verdict:"):]), "*_ "))
			if relationship == "" {
				switch verdict {
				case "yes":
					relationship = "contradiction"
				case "no":
					relationship = "unrelated"
				}
			}
		case strings.HasPrefix(lower, "explanation:"):
			// TrimLeft only — preserve any intentional * inside the explanation text.
			explanation = strings.TrimLeft(strings.TrimSpace(trimmed[len("explanation:"):]), "*_ ")
		case strings.HasPrefix(lower, "category:"):
			category = strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("category:"):]), "*_ "))
		case strings.HasPrefix(lower, "severity:"):
			severity = strings.ToLower(strings.Trim(strings.TrimSpace(trimmed[len("severity:"):]), "*_ "))
		}
	}

	if relationship == "" {
		return ValidationResult{}, fmt.Errorf("validator: no RELATIONSHIP or VERDICT line found in response")
	}

	// Normalize: strip any brackets or extra text (e.g. "[contradiction]" → "contradiction").
	relationship = strings.Trim(relationship, "[] ")

	// Normalize common LLM truncations to their canonical form.
	// Some models shorten "refinement" → "refine", "supersession" → "supersede", etc.
	switch relationship {
	case "refine":
		relationship = "refinement"
	case "supersede":
		relationship = "supersession"
	case "contradict":
		relationship = "contradiction"
	case "complement":
		relationship = "complementary"
	}

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

// NoopValidator marks candidates as unvalidated when no LLM is configured.
// Returns "unvalidated" relationship so downstream consumers can distinguish
// between LLM-confirmed conflicts and candidates that were never classified.
// Previously returned "contradiction" which caused 100% false positive rate
// for deployments without an LLM provider.
type NoopValidator struct{}

func (NoopValidator) Validate(_ context.Context, _ ValidateInput) (ValidationResult, error) {
	return ValidationResult{
		Relationship: "unvalidated",
		Explanation:  "no LLM validator configured — candidate requires manual review or LLM validation",
	}, nil
}

// perCallTimeout is the maximum time for a single LLM validation call to an
// external API (OpenAI). Separate from the scorer's overall context timeout
// so one slow call doesn't block the entire scoring pass.
const perCallTimeout = 15 * time.Second

// ollamaPerCallTimeout is higher than perCallTimeout to account for local model
// cold-start on the warmup call (model must be loaded from disk on first use)
// and slower CPU/GPU inference. A 3B model on CPU can take 20-60s to produce
// its first token; subsequent calls with keep_alive=-1 are much faster.
const ollamaPerCallTimeout = 90 * time.Second

// OllamaValidator validates conflict candidates using a local Ollama chat model.
// Reuses the existing OLLAMA_URL configuration. The model should be a text
// generation model (e.g., qwen3.5:9b), not an embedding model.
type OllamaValidator struct {
	baseURL    string
	model      string
	numThreads int // 0 = let Ollama decide; >0 = cap inference to this many CPU threads
	httpClient *http.Client
}

// NewOllamaValidator creates a validator that calls Ollama's chat API.
// numThreads caps the CPU threads Ollama uses per inference call (0 = Ollama default).
// Recommended: floor(runtime.NumCPU()/3) to leave headroom for the server and embeddings.
func NewOllamaValidator(baseURL, model string, numThreads int) *OllamaValidator {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaValidator{
		baseURL:    baseURL,
		model:      model,
		numThreads: numThreads,
		httpClient: &http.Client{
			// HTTP timeout must exceed ollamaPerCallTimeout to avoid a
			// transport-level close before the context deadline fires.
			Timeout: ollamaPerCallTimeout + 5*time.Second,
		},
	}
}

// ollamaOpts returns the options object for Ollama requests, or nil if no
// options need to be set (e.g. numThreads == 0 means use Ollama's default).
func (v *OllamaValidator) ollamaOpts() *ollamaOptions {
	if v.numThreads > 0 {
		return &ollamaOptions{NumThread: v.numThreads}
	}
	return nil
}

// Warmup loads the model into Ollama's memory before the first real validation
// call. Without this, the first backfill request pays the full cold-start
// penalty (model load from disk) which can exceed 60s on CPU. Warmup sends a
// minimal prompt; the response is discarded. It is non-fatal if it fails.
func (v *OllamaValidator) Warmup(ctx context.Context) error {
	warmCtx, cancel := context.WithTimeout(ctx, ollamaPerCallTimeout)
	defer cancel()

	body, _ := json.Marshal(ollamaChatRequest{
		Model:     v.model,
		Messages:  []ollamaChatMessage{{Role: "user", Content: "hi"}},
		Stream:    false,
		KeepAlive: "72h",
		Options:   v.ollamaOpts(),
	})
	req, err := http.NewRequestWithContext(warmCtx, http.MethodPost, v.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ollama warmup: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama warmup: request: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama warmup: status %d", resp.StatusCode)
	}
	return nil
}

type ollamaChatRequest struct {
	Model     string              `json:"model"`
	Messages  []ollamaChatMessage `json:"messages"`
	Stream    bool                `json:"stream"`
	KeepAlive string              `json:"keep_alive,omitempty"` // "72h" keeps model in RAM for 3 days (effectively permanent for dev sessions).
	Options   *ollamaOptions      `json:"options,omitempty"`
}

type ollamaOptions struct {
	NumThread int `json:"num_thread,omitempty"` // CPU threads to use for inference. 0 = Ollama default (all cores).
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
	callCtx, cancel := context.WithTimeout(ctx, ollamaPerCallTimeout)
	defer cancel()

	prompt := formatPrompt(input)

	body, err := json.Marshal(ollamaChatRequest{
		Model: v.model,
		Messages: []ollamaChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream:    false,
		KeepAlive: "72h", // Keep model loaded in RAM between calls; avoids cold-start penalty.
		Options:   v.ollamaOpts(),
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
