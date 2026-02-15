// Command rehash-content-hashes is a one-time migration script that recomputes
// content_hash for all decisions in the database. Run this after fixing the
// timestamp precision bug (Go nanoseconds vs PostgreSQL microseconds).
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./scripts/rehash-content-hashes
//
// The script connects to the database, reads every decision's canonical fields,
// recomputes the hash using the current algorithm (which truncates valid_from
// to microsecond precision), and updates any rows where the stored hash differs.
// It prints the number of rows fixed and exits.
//
// Safe to run multiple times — it's idempotent. Once all hashes match, it
// reports 0 updates and exits immediately.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/ashita-ai/akashi/internal/integrity"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	_ = godotenv.Load()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT id, decision_type, outcome, confidence, reasoning, valid_from, content_hash
		 FROM decisions
		 ORDER BY created_at ASC`)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type staleRow struct {
		id           uuid.UUID
		decisionType string
		outcome      string
		confidence   float32
		reasoning    *string
		validFrom    time.Time
	}

	var stale []staleRow
	var total int
	for rows.Next() {
		var (
			id           uuid.UUID
			decisionType string
			outcome      string
			confidence   float32
			reasoning    *string
			validFrom    time.Time
			storedHash   string
		)
		if err := rows.Scan(&id, &decisionType, &outcome, &confidence, &reasoning, &validFrom, &storedHash); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		total++
		expected := integrity.ComputeContentHash(id, decisionType, outcome, confidence, reasoning, validFrom)
		if storedHash != expected {
			stale = append(stale, staleRow{id, decisionType, outcome, confidence, reasoning, validFrom})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}

	fmt.Printf("scanned %d decisions, %d have stale hashes\n", total, len(stale))

	if len(stale) == 0 {
		fmt.Println("nothing to do")
		return nil
	}

	updated := 0
	for _, r := range stale {
		expected := integrity.ComputeContentHash(r.id, r.decisionType, r.outcome, r.confidence, r.reasoning, r.validFrom)
		tag, err := pool.Exec(ctx,
			`UPDATE decisions SET content_hash = $1 WHERE id = $2`,
			expected, r.id)
		if err != nil {
			log.Printf("update %s: %v", r.id, err)
			continue
		}
		if tag.RowsAffected() > 0 {
			updated++
		}
	}

	fmt.Printf("updated %d/%d stale hashes\n", updated, len(stale))
	return nil
}
