// Binary akashi-local runs a self-contained MCP server backed by SQLite.
//
// It requires no external infrastructure (no Docker, no Postgres, no Qdrant,
// no Ollama) and starts in under 3 seconds. The database is stored at
// ~/.akashi/local.db by default (configurable via AKASHI_DB_PATH).
//
// All MCP messages flow over stdin/stdout (stdio transport). Logs go to stderr.
//
// See ADR-009 for the architectural rationale.
package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/autoassess"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/storage/sqlite"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	// Logs go to stderr — stdout is reserved for MCP JSON-RPC.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx := context.Background()

	// Determine database path.
	dbPath := os.Getenv("AKASHI_DB_PATH")
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Error("cannot determine home directory", "error", err)
			return 1
		}
		dbPath = filepath.Join(home, ".akashi", "local.db")
	}

	// Open SQLite database.
	db, err := sqlite.New(ctx, dbPath, logger)
	if err != nil {
		logger.Error("database open failed", "error", err)
		return 1
	}
	defer db.Close(ctx)

	if err := db.EnsureDefaultOrg(ctx); err != nil {
		logger.Error("ensure default org failed", "error", err)
		return 1
	}

	// Noop embedder — semantic embedding generation requires an external provider.
	// Text search via FTS5 always works. When decisions have embeddings (e.g. stored
	// via SDK with pre-computed vectors), the LocalSearcher provides cosine similarity.
	embedder := embedding.NewNoopProvider(1536)

	// LocalSearcher: brute-force cosine similarity over SQLite-stored embeddings.
	// Always healthy (no external deps). Falls back to text search when no embeddings exist.
	searcher := search.NewLocalSearcher(db.RawDB())

	// LiteScorer: text-based conflict detection using claim extraction + word overlap.
	// No embeddings or LLM required — catches obvious contradictions between decisions.
	conflictScorer := conflicts.NewLiteScorer(db.RawDB(), logger)

	decisionSvc := decisions.New(db, embedder, searcher, logger, conflictScorer)

	// Auto-assessor for generating assessments from observable signals.
	assessor := autoassess.New(db, logger)
	decisionSvc.SetAutoAssessor(assessor)

	// nil grantCache: no caching needed for single-user local mode.
	mcpSrv := mcp.New(db, decisionSvc, nil, logger, version, 0.85, nil)
	mcpSrv.SetAutoAssessor(assessor)

	// Fixed local identity: platform_admin on the default org.
	// This bypasses all RBAC checks and gives full access.
	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.Nil.String(),
			Issuer:  "akashi-local",
		},
		AgentID: "local",
		OrgID:   uuid.Nil,
		Role:    model.RolePlatformAdmin,
	}

	logger.Info("akashi-local starting",
		"version", version,
		"db", dbPath,
	)

	// ServeStdio handles signal setup (SIGTERM, SIGINT) internally.
	if err := mcpserver.ServeStdio(
		mcpSrv.MCPServer(),
		mcpserver.WithStdioContextFunc(func(ctx context.Context) context.Context {
			return ctxutil.WithClaims(ctx, claims)
		}),
	); err != nil {
		logger.Error("stdio server error", "error", err)
		return 1
	}

	return 0
}
