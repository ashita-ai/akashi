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

// Validator confirms whether a candidate conflict is a genuine contradiction.
// The embedding scorer finds candidates (cheap, fast); the validator confirms
// them (precise, slower). This two-stage design keeps false positives low
// without requiring an LLM call for every decision pair.
type Validator interface {
	Validate(ctx context.Context, outcomeA, outcomeB, typeA, typeB string) (confirmed bool, explanation string, err error)
}

// validationPrompt is the structured prompt sent to the LLM. It asks for a
// deterministic yes/no verdict and a one-sentence explanation. The prompt is
// designed to minimize false positives — it explicitly lists what is NOT a
// contradiction to guide the model away from the common failure mode of
// flagging "different topics" as conflicts.
const validationPrompt = `You are a contradiction detector for an AI decision audit system.

Decision A (%s): %s
Decision B (%s): %s

A GENUINE CONTRADICTION means both decisions address the SAME specific question and reach INCOMPATIBLE conclusions that cannot both be true.

NOT contradictions:
- Different findings about different aspects of the same project
- Complementary observations from different review sessions
- Decisions about entirely unrelated topics
- One decision being more detailed than another on the same topic

Respond with exactly two lines:
VERDICT: yes or no
EXPLANATION: one sentence explaining why`

// ParseValidatorResponse extracts the verdict and explanation from an LLM response.
// Returns (confirmed, explanation, error). If parsing fails, returns (false, "", err)
// to enforce fail-safe behavior: ambiguous responses are treated as rejections.
func ParseValidatorResponse(response string) (bool, string, error) {
	lines := strings.Split(strings.TrimSpace(response), "\n")

	var verdict, explanation string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "verdict:") {
			verdict = strings.TrimSpace(trimmed[len("verdict:"):])
		} else if strings.HasPrefix(lower, "explanation:") {
			explanation = strings.TrimSpace(trimmed[len("explanation:"):])
		}
	}

	if verdict == "" {
		return false, "", fmt.Errorf("validator: no VERDICT line found in response")
	}

	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case "yes":
		return true, explanation, nil
	case "no":
		return false, explanation, nil
	default:
		return false, "", fmt.Errorf("validator: unrecognized verdict %q (expected yes/no)", verdict)
	}
}

// NoopValidator always confirms candidates. This preserves the current behavior
// when no LLM is configured: embedding-scored candidates are inserted without
// validation. Users who want precision must configure an LLM model.
type NoopValidator struct{}

func (NoopValidator) Validate(_ context.Context, _, _, _, _ string) (bool, string, error) {
	return true, "", nil
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

func (v *OllamaValidator) Validate(ctx context.Context, outcomeA, outcomeB, typeA, typeB string) (bool, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	prompt := fmt.Sprintf(validationPrompt, typeA, outcomeA, typeB, outcomeB)

	body, err := json.Marshal(ollamaChatRequest{
		Model: v.model,
		Messages: []ollamaChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	})
	if err != nil {
		return false, "", fmt.Errorf("ollama validator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, v.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("ollama validator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("ollama validator: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return false, "", fmt.Errorf("ollama validator: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, "", fmt.Errorf("ollama validator: decode response: %w", err)
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

func (v *OpenAIValidator) Validate(ctx context.Context, outcomeA, outcomeB, typeA, typeB string) (bool, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	prompt := fmt.Sprintf(validationPrompt, typeA, outcomeA, typeB, outcomeB)

	body, err := json.Marshal(openAIChatRequest{
		Model: v.model,
		Messages: []openAIChatMessage{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return false, "", fmt.Errorf("openai validator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("openai validator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.apiKey)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("openai validator: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return false, "", fmt.Errorf("openai validator: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, "", fmt.Errorf("openai validator: decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return false, "", fmt.Errorf("openai validator: no choices in response")
	}

	return ParseValidatorResponse(result.Choices[0].Message.Content)
}
