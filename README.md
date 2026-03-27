[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791.svg)](https://www.postgresql.org/)

**Version control for AI decisions.**

Multi-agent AI systems are moving from demos to production, but their decisions are invisible and uncoordinated. Agents contradict each other, relitigate settled work, and have no shared memory of what's already been decided. When something goes wrong, nobody can answer: *who decided what, when, why, and what alternatives were considered?*

Akashi is the decision coordination layer. Every agent checks for precedents before deciding and records its full reasoning after. When agents diverge on the same topic, Akashi detects it semantically — and when the CTO asks "why did the AI do that?" or an auditor asks for proof of decision traceability, you have the answer.

![Akashi dashboard showing decision audit trail, agent coordination health, and conflict detection](docs/images/dashboard.png)

## How it works

Akashi is built around two primitives: **check before deciding, trace after deciding.**

```
Before making a decision          After making a decision
─────────────────────────         ───────────────────────
akashi_check                      akashi_trace
  "has anyone decided this?"        "here's what I decided and why"
  → precedents                      → stored permanently
  → known conflicts                 → embeddings computed
                                    → conflicts detected
```

When an agent calls `akashi_trace`, the decision is written atomically with its reasoning, alternatives, and evidence. Embeddings are computed, and conflict detection runs asynchronously — comparing the new decision against the org's history to find genuine contradictions between agents. Conflicts have a lifecycle (`open → resolved` or `false_positive`) and can declare a winner when resolved.

When an agent later observes whether a past decision was correct, `akashi_assess` feeds that outcome back into search re-ranking — so better decisions surface higher as precedents over time.

See [Subsystems](docs/subsystems.md) and [Conflict Detection](docs/conflicts.md) for internals.

---

## Quick start

Two modes are available today. A third (local-lite, zero-infrastructure) is in progress.

### Complete local stack (recommended)

> **Start here.** This is the fastest path to a fully working Akashi with all features.

Everything runs in Docker — TimescaleDB, Qdrant, Ollama, and the Akashi server. No API keys, no external accounts.

```bash
docker compose -f docker-compose.complete.yml up -d
```

**First launch builds the server image from source and downloads two Ollama models: `mxbai-embed-large` (~670MB) for embeddings and `qwen3.5:9b` (~6.6GB) for LLM conflict validation.** Expect 15–25 minutes on first run depending on your machine and network. Subsequent launches start instantly.

---

## Contributing

We welcome contributions to Akashi! To get started:
1. **Fork** the repository and create your branch from `main`.
2. **Setup**: Run `make dev-deps` to spin up local databases.
3. **Test**: Ensure all tests pass using `go test ./...`.
4. **Submit**: Open a Pull Request with a clear description of your changes.

For major architectural changes, please open an issue first to discuss your proposal.

## License

Akashi is released under the [Apache License 2.0](LICENSE).