// Akashi Go SDK quickstart — check, trace, and query decisions.
//
// Prerequisites:
//
//	docker compose -f docker-compose.complete.yml up -d
//	cd examples/go && go mod tidy
//
// Run:
//
//	cd examples/go && go run ./quickstart
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ashita-ai/akashi/sdk/go/akashi"
)

func ptr(s string) *string { return &s }

func main() {
	url := envOr("AKASHI_URL", "http://localhost:8080")
	adminKey := envOr("AKASHI_ADMIN_API_KEY", "admin")
	ctx := context.Background()

	// --- Connect as admin and verify the server is up ---
	admin, err := akashi.NewClient(akashi.Config{
		BaseURL: url, AgentID: "admin", APIKey: adminKey,
	})
	if err != nil {
		log.Fatal(err)
	}
	health, err := admin.Health(ctx)
	if err != nil {
		log.Fatal("Cannot reach Akashi server:", err)
	}
	fmt.Printf("==> Connected to Akashi %s (postgres: %s)\n", health.Version, health.Postgres)

	// --- Create a demo agent (idempotent — ignores 409 if it already exists) ---
	_, err = admin.CreateAgent(ctx, akashi.CreateAgentRequest{
		AgentID: "quickstart-agent-go",
		Name:    "Quickstart Agent (Go)",
		Role:    akashi.RoleAgent,
		APIKey:  "quickstart-secret-go",
	})
	if err != nil && !akashi.IsConflict(err) {
		log.Fatal("Failed to create agent:", err)
	}
	if akashi.IsConflict(err) {
		fmt.Println("==> Agent 'quickstart-agent-go' already exists")
	} else {
		fmt.Println("==> Created agent 'quickstart-agent-go'")
	}

	// --- Switch to the agent identity ---
	client, err := akashi.NewClient(akashi.Config{
		BaseURL: url, AgentID: "quickstart-agent-go", APIKey: "quickstart-secret-go",
	})
	if err != nil {
		log.Fatal(err)
	}

	// --- Check: are there existing decisions about model selection? ---
	fmt.Println("\n==> Checking for precedents on 'model_selection'...")
	check, err := client.Check(ctx, akashi.CheckRequest{DecisionType: "model_selection"})
	if err != nil {
		log.Fatal("Check failed:", err)
	}
	if check.HasPrecedent {
		fmt.Printf("    Found %d prior decision(s)\n", len(check.Decisions))
		for _, d := range check.Decisions {
			fmt.Printf("    - %s (confidence=%.2f)\n", d.Outcome, d.Confidence)
		}
	} else {
		fmt.Println("    No prior decisions found — this will be the first.")
	}

	// --- Trace: record a new decision ---
	fmt.Println("\n==> Tracing a model selection decision...")
	reasoning := "GPT-4o offers the best quality-to-cost ratio for summarization. " +
		"Benchmarked against Claude and Gemini on 200 sample documents."
	rejClaude := "Slightly higher latency on long documents"
	rejGemini := "Inconsistent formatting in structured output"
	scoreClaude := float32(0.80)
	scoreGPT := float32(0.85)
	scoreGemini := float32(0.70)
	relevance := float32(0.9)

	resp, err := client.Trace(ctx, akashi.TraceRequest{
		DecisionType: "model_selection",
		Outcome:      "Use GPT-4o for summarization tasks",
		Confidence:   0.85,
		Reasoning:    &reasoning,
		Alternatives: []akashi.TraceAlternative{
			{Label: "GPT-4o", Score: &scoreGPT, Selected: true},
			{Label: "Claude 3.5 Sonnet", Score: &scoreClaude, Selected: false, RejectionReason: &rejClaude},
			{Label: "Gemini 1.5 Pro", Score: &scoreGemini, Selected: false, RejectionReason: &rejGemini},
		},
		Evidence: []akashi.TraceEvidence{
			{
				SourceType:     "benchmark",
				Content:        "GPT-4o ROUGE-L: 0.47, Claude: 0.45, Gemini: 0.41 on CNN/DailyMail",
				RelevanceScore: &relevance,
			},
		},
	})
	if err != nil {
		log.Fatal("Trace failed:", err)
	}
	fmt.Printf("    Decision recorded: id=%s\n", resp.DecisionID)

	// --- Query: retrieve decisions matching our filter ---
	fmt.Println("\n==> Querying model_selection decisions...")
	query, err := client.Query(ctx, &akashi.QueryFilters{DecisionType: ptr("model_selection")}, nil)
	if err != nil {
		log.Fatal("Query failed:", err)
	}
	fmt.Printf("    Found %d decision(s)\n", query.Total)
	for _, d := range query.Decisions {
		fmt.Printf("    - [%s] %s (confidence=%.2f)\n", d.AgentID, d.Outcome, d.Confidence)
	}

	// --- Recent: fetch the latest decisions across all types ---
	fmt.Println("\n==> Fetching 5 most recent decisions...")
	recent, err := client.Recent(ctx, &akashi.RecentOptions{Limit: 5})
	if err != nil {
		log.Fatal("Recent failed:", err)
	}
	fmt.Printf("    Got %d decision(s)\n", len(recent))
	for _, d := range recent {
		fmt.Printf("    - [%s] %s\n", d.DecisionType, d.Outcome)
	}

	fmt.Printf("\n==> Done. View your decisions at %s\n", url)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
