package mcp

import (
	"context"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerPrompts() {
	// before-decision — guides the agent through checking precedents first.
	s.mcpServer.AddPrompt(
		mcplib.NewPrompt("before-decision",
			mcplib.WithPromptDescription("Guide for checking precedents before making a decision"),
			mcplib.WithArgument("decision_type",
				mcplib.ArgumentDescription("The type of decision you're about to make (e.g., model_selection, architecture, data_source)"),
				mcplib.RequiredArgument(),
			),
		),
		s.handleBeforeDecisionPrompt,
	)

	// after-decision — reminds the agent to record what was decided.
	s.mcpServer.AddPrompt(
		mcplib.NewPrompt("after-decision",
			mcplib.WithPromptDescription("Reminder to record a decision after making it"),
			mcplib.WithArgument("decision_type",
				mcplib.ArgumentDescription("The type of decision that was made"),
				mcplib.RequiredArgument(),
			),
			mcplib.WithArgument("outcome",
				mcplib.ArgumentDescription("What was decided"),
				mcplib.RequiredArgument(),
			),
		),
		s.handleAfterDecisionPrompt,
	)

	// agent-setup — full system prompt snippet explaining the Kyoyu workflow.
	s.mcpServer.AddPrompt(
		mcplib.NewPrompt("agent-setup",
			mcplib.WithPromptDescription("System prompt snippet explaining the Kyoyu check-before/record-after workflow"),
		),
		s.handleAgentSetupPrompt,
	)
}

func (s *Server) handleBeforeDecisionPrompt(ctx context.Context, request mcplib.GetPromptRequest) (*mcplib.GetPromptResult, error) {
	decisionType := request.Params.Arguments["decision_type"]
	if decisionType == "" {
		return nil, fmt.Errorf("decision_type argument is required")
	}

	return &mcplib.GetPromptResult{
		Description: fmt.Sprintf("Check precedents before making a %s decision", decisionType),
		Messages: []mcplib.PromptMessage{
			{
				Role: mcplib.RoleUser,
				Content: mcplib.TextContent{
					Type: "text",
					Text: fmt.Sprintf(`Before making this %s decision, follow these steps:

1. CALL kyoyu_check with decision_type="%s" to look for existing precedents.

2. REVIEW the response:
   - If has_precedent is true, read the prior decisions carefully.
     Build on them rather than contradicting them, unless you have strong reason to diverge.
   - If conflicts exist, acknowledge them explicitly and explain how your
     decision resolves or avoids the conflict.
   - If has_precedent is false, you're breaking new ground. Be especially
     thorough in your reasoning.

3. MAKE your decision, incorporating what you learned from precedents.

4. RECORD your decision by calling kyoyu_trace with:
   - decision_type="%s"
   - outcome: what you decided (be specific)
   - confidence: your certainty (0.0-1.0)
   - reasoning: why you chose this, referencing precedents if applicable`, decisionType, decisionType, decisionType),
				},
			},
		},
	}, nil
}

func (s *Server) handleAfterDecisionPrompt(ctx context.Context, request mcplib.GetPromptRequest) (*mcplib.GetPromptResult, error) {
	decisionType := request.Params.Arguments["decision_type"]
	outcome := request.Params.Arguments["outcome"]
	if decisionType == "" || outcome == "" {
		return nil, fmt.Errorf("decision_type and outcome arguments are required")
	}

	return &mcplib.GetPromptResult{
		Description: fmt.Sprintf("Record your %s decision", decisionType),
		Messages: []mcplib.PromptMessage{
			{
				Role: mcplib.RoleUser,
				Content: mcplib.TextContent{
					Type: "text",
					Text: fmt.Sprintf(`You just made a decision. Record it now so other agents can learn from it.

CALL kyoyu_trace with:
- decision_type: "%s"
- outcome: "%s"
- confidence: your certainty about this decision (0.0-1.0). Be honest.
- reasoning: explain your chain of thought. What alternatives did you consider?
  What evidence supported this choice? What trade-offs did you accept?

Good reasoning includes:
- Why this option over alternatives
- What constraints or requirements drove the choice
- What risks you accepted
- References to prior decisions that influenced you`, decisionType, outcome),
				},
			},
		},
	}, nil
}

func (s *Server) handleAgentSetupPrompt(ctx context.Context, request mcplib.GetPromptRequest) (*mcplib.GetPromptResult, error) {
	return &mcplib.GetPromptResult{
		Description: "Kyoyu decision-sharing workflow for AI agents",
		Messages: []mcplib.PromptMessage{
			{
				Role: mcplib.RoleUser,
				Content: mcplib.TextContent{
					Type: "text",
					Text: `You have access to Kyoyu, a decision-sharing system that helps AI agents
coordinate by recording and querying past decisions.

## The Pattern: Check Before, Record After

Every non-trivial decision should follow this workflow:

### Before deciding:
Call kyoyu_check with the decision_type you're about to make.
This returns existing precedents and any conflicts. Use this information
to make a more informed decision and avoid contradicting prior work.

### After deciding:
Call kyoyu_trace to record what you decided, why, and how confident you are.
This creates a searchable record that other agents (and your future self)
can learn from.

## Available Tools

- kyoyu_check: Look for existing decisions before making a new one (use FIRST)
- kyoyu_trace: Record a decision you just made (use AFTER deciding)
- kyoyu_query: Find decisions by exact filters (type, agent, confidence)
- kyoyu_search: Find decisions by semantic similarity (natural language)
- kyoyu_recent: See what's been decided recently (good for context)

## Decision Types

Use standard categories when possible:
- model_selection: Choosing AI models, parameters, or configurations
- architecture: System design, patterns, infrastructure choices
- data_source: Where to get data, which datasets, data formats
- error_handling: How to handle failures, retries, fallbacks
- feature_scope: What to include/exclude, prioritization
- trade_off: Explicit trade-off resolutions (speed vs accuracy, etc.)
- deployment: Deployment strategy, environments, rollout plans
- security: Authentication, authorization, encryption choices

## Confidence Levels

Be honest about your confidence:
- 0.9-1.0: Near-certain, strong evidence, well-established pattern
- 0.7-0.8: Confident, good reasoning, some uncertainty remains
- 0.5-0.6: Moderate, reasonable choice but alternatives are viable
- 0.3-0.4: Low confidence, making a judgment call with limited info
- 0.1-0.2: Best guess, would welcome revision with more data`,
				},
			},
		},
	}, nil
}
