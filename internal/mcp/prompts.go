package mcp

import (
	"context"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerPrompts() {
	// before-decision — guides the agent through checking the black box for precedents first.
	s.mcpServer.AddPrompt(
		mcplib.NewPrompt("before-decision",
			mcplib.WithPromptDescription("Check the black box for precedents before making a decision"),
			mcplib.WithArgument("decision_type",
				mcplib.ArgumentDescription("The type of decision you're about to make (e.g., model_selection, architecture, data_source)"),
				mcplib.RequiredArgument(),
			),
		),
		s.handleBeforeDecisionPrompt,
	)

	// after-decision — reminds the agent to record the decision to the black box.
	s.mcpServer.AddPrompt(
		mcplib.NewPrompt("after-decision",
			mcplib.WithPromptDescription("Record a decision to the black box after making it"),
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

	// agent-setup — full system prompt snippet explaining the Akashi decision audit workflow.
	s.mcpServer.AddPrompt(
		mcplib.NewPrompt("agent-setup",
			mcplib.WithPromptDescription("System prompt snippet explaining the Akashi decision audit workflow (check-before/record-after)"),
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

1. CALL akashi_check with decision_type="%s" to look for existing precedents.

2. REVIEW the response:
   - If has_precedent is true, read the prior decisions carefully.
     Build on them rather than contradicting them, unless you have strong reason to diverge.
   - If conflicts exist, acknowledge them explicitly and explain how your
     decision resolves or avoids the conflict.
   - If has_precedent is false, you're breaking new ground. Be especially
     thorough in your reasoning.

3. MAKE your decision, incorporating what you learned from precedents.

4. RECORD your decision by calling akashi_trace with:
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
					Text: fmt.Sprintf(`You just made a decision. Record it to the black box now so there is proof of why, and other agents can learn from it.

CALL akashi_trace with:
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
		Description: "Akashi decision audit workflow for AI agents",
		Messages: []mcplib.PromptMessage{
			{
				Role: mcplib.RoleUser,
				Content: mcplib.TextContent{
					Type: "text",
					Text: `You have access to Akashi, a black box recorder for AI decisions. It captures
why every decision was made -- what was chosen, what was rejected, and what
evidence supported it -- so there is always proof when someone asks "why?"

## The Pattern: Check Before, Record After

Every non-trivial decision should follow this workflow:

### Before deciding:
Call akashi_check with the decision_type you're about to make.
This checks the black box for existing precedents and any conflicts.
Use this information to make a more informed decision and avoid
contradicting prior work.

### After deciding:
Call akashi_trace to record what you decided, why, and how confident you are.
This adds to the audit trail so other agents (and your future self)
can learn from it, and so the decision is provable later.

## Available Tools

- akashi_check: Check the black box for precedents before deciding (use FIRST)
- akashi_trace: Record a decision to the black box (use AFTER deciding)
- akashi_query: Query the audit trail by exact filters (type, agent, confidence)
- akashi_search: Search the black box by semantic similarity (natural language)
- akashi_recent: See what the black box recorded recently (good for context)

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
