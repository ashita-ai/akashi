# Project Structure

Generated: $(date -u +%Y-%m-%d)

```
./api/embed.go
./cmd/akashi/main.go
./internal/auth/auth.go
./internal/auth/hash.go
./internal/billing/billing.go
./internal/billing/metering.go
./internal/billing/webhooks.go
./internal/config/config.go
./internal/ctxutil/ctxutil.go
./internal/mcp/mcp.go
./internal/mcp/prompts.go
./internal/mcp/resources.go
./internal/mcp/tools.go
./internal/model/agent.go
./internal/model/api.go
./internal/model/decision.go
./internal/model/event.go
./internal/model/query.go
./internal/model/run.go
./internal/ratelimit/middleware.go
./internal/ratelimit/ratelimit.go
./internal/server/authz.go
./internal/server/broker.go
./internal/server/handlers.go
./internal/server/handlers_admin.go
./internal/server/handlers_billing.go
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
./internal/signup/signup.go
./internal/storage/agents.go
./internal/storage/alternatives.go
./internal/storage/conflicts.go
./internal/storage/decisions.go
./internal/storage/delete.go
./internal/storage/events.go
./internal/storage/evidence.go
./internal/storage/grants.go
./internal/storage/migrate.go
./internal/storage/notify.go
./internal/storage/organizations.go
./internal/storage/pool.go
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
|  | 2 |
| github.com/ashita-ai/akashi/internal/billing | 7 |
| github.com/ashita-ai/akashi/internal/signup | 16 |
| github.com/ashita-ai/akashi/internal/service/embedding | 2 |
| github.com/ashita-ai/akashi/internal/ratelimit | 39 |
| github.com/ashita-ai/akashi/internal/service/quality | 3 |
| github.com/ashita-ai/akashi/internal/server | 2 |
| github.com/ashita-ai/akashi/internal/auth | 8 |
