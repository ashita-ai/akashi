# Akashi CrewAI Demo

Two AI agents independently reason about a shared problem. They reach
conflicting conclusions on the same day. Akashi catches it automatically —
no human had to compare notes.

Each run picks a **random scenario** from a pool of four, so the conflict
you see (and the agents involved) differ every time.

```
  Scenario: model_selection

┌─ ml-engineer ─────────────────────────────────────────────────────────┐
│ Fine-tune Llama 3.1 70B on our domain corpus and self-host on 2× A100 │
│ instances. Projects 94% accuracy, eliminates $180K/year API costs,    │
│ and keeps PHI on-prem for HIPAA.                                       │
└───────────────────────────────────────────────────────────────────────┘

┌─ product-manager ─────────────────────────────────────────────────────┐
│ Use GPT-4o via the OpenAI API with a HIPAA BAA. Ships in 2 weeks,     │
│ no GPU infrastructure, no fine-tuning pipeline to maintain.           │
└───────────────────────────────────────────────────────────────────────┘

⚠ CONFLICT DETECTED  topic_similarity=0.81  outcome_divergence=0.41
  LLM: "proposals advocate mutually exclusive model strategies"
```

## Quick start

**Requirements:** Akashi server running at `http://localhost:8080`.
See the root `docker-compose.yml` or `docker-compose.complete.yml`.

```sh
# 1. Create venv and install dependencies
python3 -m venv .venv
source .venv/bin/activate
pip install crewai httpx

# Also install the Akashi SDK and CrewAI integration from the local tree:
pip install -e ../../sdk/python
pip install -e ../../sdk/integrations/crewai

# 2. Run the demo (no API key needed — random scenario, pre-scripted responses)
# For docker-compose.complete: export AKASHI_ADMIN_API_KEY=admin (matches default)
python demo.py

# Force a specific scenario (0–3):
python demo.py --scenario 0   # payment service architecture
python demo.py --scenario 1   # LLM model selection
python demo.py --scenario 2   # observability database
python demo.py --scenario 3   # service-to-service auth
```

## Live mode (real LLM agents)

```sh
OPENAI_API_KEY=sk-... python demo.py --live
OPENAI_API_KEY=sk-... python demo.py --live --scenario 2
```

With `--live`, two CrewAI agents make real LLM calls. Each scenario
defines role, goal, and backstory for both agents so their biases
produce genuine disagreement. The conflict detection outcome is the
same; the reasoning is generated on the fly.

## Docker (docker-compose.complete.yml)

For the all-in-one stack with Qdrant + Ollama:

1. **First launch:** Models take 5–15 min to pull. Wait for `ollama-init`:
   ```sh
   docker compose -f docker-compose.complete.yml logs -f ollama-init
   # Wait for "mxbai-embed-large: ready" and "qwen2.5:3b: ready"
   ```

2. **Run the demo:**
   ```sh
   export AKASHI_ADMIN_API_KEY=admin   # matches docker default
   python demo.py --scenario 2         # pre-scripted (recommended first)
   OPENAI_API_KEY=sk-... python demo.py --live --scenario 2
   ```

3. **"No conflict" or "search_enabled=false"?** Embeddings weren't ready. Re-check
   step 1. The demo now fails fast with a clear message if semantic search is disabled.

## Scenarios

| # | ID | Agents | Decision type |
|---|-----|--------|--------------|
| 0 | `payment_architecture` | systems-architect vs senior-engineer | architecture |
| 1 | `model_selection` | ml-engineer vs product-manager | model_selection |
| 2 | `observability_database` | data-engineer vs sre-lead | architecture |
| 3 | `service_auth` | security-architect vs platform-engineer | security |

## How it works

1. Agent A analyzes the scenario → traces to Akashi using the admin JWT,
   with `agent_id` set to the scenario's first persona.
2. A 4-second delay lets Qdrant index the first decision.
3. Agent B analyzes the same scenario independently → traces to Akashi
   with `agent_id` set to the scenario's second persona.
4. Akashi's conflict detector runs:
   - Embeds both decisions, runs Qdrant ANN to find similar past decisions
   - Claim-level scoring (topic similarity ≥ 0.7 threshold)
   - LLM validator classifies the relationship as a contradiction
5. The demo polls `GET /v1/conflicts?agent_id=<agent_a>` until the
   conflict appears (typically 10–20 seconds).
6. View and resolve the conflict at `http://localhost:8080/conflicts`.

## Why the SDK isn't used for tracing

The Python SDK (`AkashiSyncClient`) ties `agent_id` to the authenticated
identity — you'd need a separate API key per agent. Instead, this demo
uses raw `httpx` with the admin JWT, which allows tracing on behalf of
any agent identity (`admin` role bypasses the agent_id match check).
This is the right pattern for orchestration code that manages multiple
AI agents from a single privileged context.
