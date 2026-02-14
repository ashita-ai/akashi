# Project Structure

```
./api/embed.go
./cmd/akashi/main.go
./migrations/embed.go
./internal/auth/auth.go
./internal/auth/hash.go
./internal/conflicts/scorer.go
./internal/authz/authz.go
./internal/config/config.go
./internal/ctxutil/ctxutil.go
./internal/integrity/integrity.go
./internal/mcp/mcp.go
./internal/mcp/prompts.go
./internal/mcp/resources.go
./internal/mcp/tracker.go
./internal/mcp/tracker_test.go
./internal/mcp/tools.go
./internal/model/agent.go
./internal/model/api.go
./internal/model/decision.go
./internal/model/event.go
./internal/model/query.go
./internal/model/run.go
./internal/ratelimit/memory.go
./internal/ratelimit/ratelimit.go
./internal/search/outbox.go
./internal/search/qdrant.go
./internal/search/search.go
./internal/server/authz.go
./internal/server/broker.go
./internal/server/handlers.go
./internal/server/handlers_admin.go
./internal/server/handlers_decisions.go
./internal/server/handlers_export.go
./internal/server/handlers_runs.go
./internal/server/middleware.go
./internal/server/server.go
./internal/server/spa.go
./internal/service/decisions/service.go
./internal/service/embedding/embedding.go
./internal/service/embedding/ollama.go
./internal/service/quality/quality.go
./internal/service/trace/buffer.go
./internal/storage/agents.go
./internal/storage/alternatives.go
./internal/storage/conflicts.go
./internal/storage/decisions.go
./internal/storage/delete.go
./internal/storage/errors.go
./internal/storage/events.go
./internal/storage/evidence.go
./internal/storage/grants.go
./internal/storage/integrity.go
./internal/storage/migrate.go
./internal/storage/notify.go
./internal/storage/organizations.go
./internal/storage/pool.go
./internal/storage/retry.go
./internal/storage/runs.go
./internal/storage/trace.go
./internal/telemetry/telemetry.go
./sdk/go/akashi/auth.go
./sdk/go/akashi/client.go
./sdk/go/akashi/errors.go
./sdk/go/akashi/types.go
./ui/ui.go
./ui/ui_noop.go
```

## Test Summary

| Package | Tests |
|---------|-------|
| github.com/ashita-ai/akashi/internal/auth | 5 |
| github.com/ashita-ai/akashi/internal/conflicts | 3 |
| github.com/ashita-ai/akashi/internal/config | 10 |
| github.com/ashita-ai/akashi/internal/integrity | 12 |
| github.com/ashita-ai/akashi/internal/mcp | 1 |
| github.com/ashita-ai/akashi/internal/model | 2 |
| github.com/ashita-ai/akashi/internal/ratelimit | 10 |
| github.com/ashita-ai/akashi/internal/search | 6 |
| github.com/ashita-ai/akashi/internal/server | 35 |
| github.com/ashita-ai/akashi/internal/service/embedding | 3 |
| github.com/ashita-ai/akashi/internal/service/quality | 29 |
| github.com/ashita-ai/akashi/internal/service/trace | 1 |
| github.com/ashita-ai/akashi/internal/storage | 21 |
| github.com/ashita-ai/akashi/sdk/go/akashi | 30 |
