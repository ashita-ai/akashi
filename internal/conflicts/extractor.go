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

// ClaimCategory classifies the role of a claim in a decision outcome.
type ClaimCategory string

const (
	ClaimFinding        ClaimCategory = "finding"
	ClaimRecommendation ClaimCategory = "recommendation"
	ClaimAssessment     ClaimCategory = "assessment"
	ClaimStatus         ClaimCategory = "status"
)

// validClaimCategories defines the allowed values for claim classification.
var validClaimCategories = map[string]bool{
	"finding":        true,
	"recommendation": true,
	"assessment":     true,
	"status":         true,
}

// ConflictRelevantCategory returns true if the category participates in conflict scoring.
func ConflictRelevantCategory(category *string) bool {
	if category == nil {
		return true // uncategorized claims (regex-extracted) always participate
	}
	return *category == string(ClaimFinding) || *category == string(ClaimAssessment)
}

// ExtractedClaim is a structured claim extracted by the LLM.
type ExtractedClaim struct {
	Text     string `json:"text"`
	Category string `json:"category"`
}

// ClaimExtractor extracts structured claims from decision outcomes.
type ClaimExtractor interface {
	ExtractClaims(ctx context.Context, outcome string) ([]ExtractedClaim, error)
}

// extractorPrompt is the system prompt for LLM-based claim extraction.
const extractorPrompt = `You are a claim extractor for an AI decision audit system.

Given a decision outcome text, extract individual claims and classify each.

CATEGORIES:
- finding: A factual observation, bug report, or discovered issue. Example: "The outbox has no deadletter mechanism."
- assessment: An evaluative judgment about quality, risk, or correctness. Example: "Security posture is strong overall."
- recommendation: A suggested action or approach. Example: "Use PostgreSQL for the primary database."
- status: A progress update or boilerplate. Example: "All tests pass." "CI is green."

RULES:
- Extract each distinct claim as a separate item.
- Each claim must be a complete, self-contained sentence.
- Drop fragments shorter than 20 characters.
- Drop pure boilerplate (e.g. "LGTM", "Ship it", "Approved").
- Preserve technical specificity — don't generalize or paraphrase.
- When a sentence contains multiple distinct claims, split them.

Respond with a JSON array only. No markdown, no explanation.
Example: [{"text":"The outbox has no deadletter mechanism","category":"finding"},{"text":"Security posture is strong","category":"assessment"}]`

// OllamaExtractor extracts claims using a local Ollama chat model.
type OllamaExtractor struct {
	baseURL    string
	model      string
	numThreads int
	httpClient *http.Client
}

// NewOllamaExtractor creates a claim extractor backed by Ollama.
func NewOllamaExtractor(baseURL, model string, numThreads int) *OllamaExtractor {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaExtractor{
		baseURL:    baseURL,
		model:      model,
		numThreads: numThreads,
		httpClient: &http.Client{
			Timeout: ollamaPerCallTimeout + 5*time.Second,
		},
	}
}

func (e *OllamaExtractor) ExtractClaims(ctx context.Context, outcome string) ([]ExtractedClaim, error) {
	callCtx, cancel := context.WithTimeout(ctx, ollamaPerCallTimeout)
	defer cancel()

	var opts *ollamaOptions
	if e.numThreads > 0 {
		opts = &ollamaOptions{NumThread: e.numThreads}
	}

	body, err := json.Marshal(ollamaChatRequest{
		Model: e.model,
		Messages: []ollamaChatMessage{
			{Role: "system", Content: extractorPrompt},
			{Role: "user", Content: outcome},
		},
		Stream:    false,
		KeepAlive: "72h",
		Options:   opts,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama extractor: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, e.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama extractor: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama extractor: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("ollama extractor: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama extractor: decode response: %w", err)
	}

	return parseExtractedClaims(result.Message.Content)
}

// OpenAIExtractor extracts claims using the OpenAI chat API.
type OpenAIExtractor struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewOpenAIExtractor creates a claim extractor backed by OpenAI.
func NewOpenAIExtractor(apiKey, model string) *OpenAIExtractor {
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &OpenAIExtractor{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: perCallTimeout + 5*time.Second,
		},
	}
}

func (e *OpenAIExtractor) ExtractClaims(ctx context.Context, outcome string) ([]ExtractedClaim, error) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	body, err := json.Marshal(openAIChatRequest{
		Model: e.model,
		Messages: []openAIChatMessage{
			{Role: "system", Content: extractorPrompt},
			{Role: "user", Content: outcome},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("openai extractor: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai extractor: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai extractor: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("openai extractor: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("openai extractor: decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("openai extractor: no choices in response")
	}

	return parseExtractedClaims(result.Choices[0].Message.Content)
}

// parseExtractedClaims parses LLM JSON output into validated claims.
func parseExtractedClaims(raw string) ([]ExtractedClaim, error) {
	// Strip markdown code fences if present.
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		// Remove first and last lines (code fences).
		if len(lines) >= 3 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	raw = strings.TrimSpace(raw)

	var claims []ExtractedClaim
	if err := json.Unmarshal([]byte(raw), &claims); err != nil {
		return nil, fmt.Errorf("extractor: parse JSON: %w (raw: %.200s)", err, raw)
	}

	// Validate and normalize.
	var valid []ExtractedClaim
	for _, c := range claims {
		c.Text = strings.TrimSpace(c.Text)
		if len(c.Text) < 20 {
			continue
		}
		c.Category = strings.ToLower(strings.TrimSpace(c.Category))
		if !validClaimCategories[c.Category] {
			c.Category = "finding" // default unknown categories to finding (safe for conflict scoring)
		}
		valid = append(valid, c)
	}

	return valid, nil
}
