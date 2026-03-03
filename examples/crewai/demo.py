#!/usr/bin/env python3
"""
Akashi CrewAI Demo — Multi-Agent Conflict Detection
====================================================
Two AI agents independently reason about a shared problem.
They reach conflicting conclusions on the same day.
Akashi catches it automatically — no human had to compare notes.

Quick start (no API key needed — picks a random scenario):
    cd examples/crewai
    python demo.py

Force a specific scenario (0–3):
    python demo.py --scenario 0

With real LLM agents (CrewAI + OpenAI):
    OPENAI_API_KEY=sk-... python demo.py --live
"""
from __future__ import annotations

import os
import random
import sys
import time
import textwrap
from pathlib import Path

import httpx

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

AKASHI_URL = "http://localhost:8080"


# ---------------------------------------------------------------------------
# Scenario pool
#
# One scenario is chosen at random each run (or forced with --scenario N).
#
# Each dict has:
#   id            str   short identifier shown in output
#   description   str   THE SCENARIO section text
#   decision_type str   stored on both traced decisions
#   agent_a/b     dict  one voice per agent — see AgentSpec shape below
#
# AgentSpec keys:
#   id, label, outcome, reasoning, confidence  — pre-scripted (default mode)
#   role, goal, backstory                      — live mode (CrewAI agents)
# ---------------------------------------------------------------------------

SCENARIOS = [
    # ── 0: Backend Architecture (payment service) ─────────────────────────
    {
        "id": "payment_architecture",
        "description": (
            "Design the backend architecture for a new payment processing service "
            "that will handle $1M/day in transactions. Consider: scalability, "
            "PCI-DSS compliance, operational complexity, and a team of 8 engineers."
        ),
        "decision_type": "architecture",
        "agent_a": {
            "id": "systems-architect",
            "label": "Systems Architect",
            "confidence": 0.88,
            "outcome": (
                "Adopt microservices architecture: payment-service, fraud-service, and "
                "notification-service as independent deployables with Kafka for async "
                "inter-service communication and a shared PCI-DSS network segment."
            ),
            "reasoning": textwrap.dedent("""\
                After evaluating the requirements — $1M/day in transactions, PCI-DSS
                compliance, and the need for independent fraud model redeployment — I
                recommend a microservices architecture.

                Key factors:
                1. Payment, fraud, and notification workloads scale at different rates.
                   Microservices let us provision each independently.
                2. PCI-DSS scope reduction: isolate card-data handling to a single
                   payment-service network segment. Monolith means entire codebase is in scope.
                3. Independent deployment cycles: fraud model updates (daily retraining)
                   should not require a full payment-service release.
                4. Kafka as async event bus provides durability, backpressure, and a
                   natural audit log of every payment event.

                Risk: higher operational complexity. Mitigate with a service mesh
                (Linkerd) and centralized observability from day one.
            """),
            "role": "Systems Architect",
            "goal": "Design scalable, compliant backend architectures for fintech systems.",
            "backstory": (
                "You are a systems architect who has built distributed payment platforms "
                "at two major fintech companies. You believe microservices are the right "
                "default for any system that needs to scale independently."
            ),
        },
        "agent_b": {
            "id": "senior-engineer",
            "label": "Senior Backend Engineer",
            "confidence": 0.90,
            "outcome": (
                "Adopt modular monolith: single deployable with payment/, fraud/, and "
                "notification/ domain packages, enforced module boundaries via linting. "
                "Extract to microservices only when profiling proves a bottleneck exists."
            ),
            "reasoning": textwrap.dedent("""\
                I've seen microservices sold as the default answer for years. At $1M/day
                (~$11/sec peak), we are nowhere near the scale that justifies distributed
                systems complexity.

                Key factors:
                1. Network latency: every cross-service call adds 5–15ms. Payment
                   authorization latency SLAs are tight — we can't afford it.
                2. Distributed transactions: payment + fraud + notification spanning
                   multiple services requires sagas or 2PC. Both are hard to get right.
                3. Team bandwidth: with 8 engineers, 40% of sprint capacity disappears
                   into service mesh, service discovery, and Kubernetes config.
                4. Premature extraction: a modular monolith with enforced domain
                   boundaries (go/src/payment/, go/src/fraud/, go/src/notification/)
                   can be split when profiling reveals an actual bottleneck.

                Start simple. Extract services when we have evidence, not speculation.
            """),
            "role": "Senior Backend Engineer",
            "goal": "Build reliable, maintainable backend systems that teams can ship fast.",
            "backstory": (
                "You are a senior backend engineer who has suffered through two failed "
                "microservices rewrites. You believe in starting simple and extracting "
                "services only when you have concrete evidence of need."
            ),
        },
    },

    # ── 1: LLM Model Selection (customer support agent) ───────────────────
    {
        "id": "model_selection",
        "description": (
            "Select the AI model strategy for a customer-support agent that will "
            "handle 50K conversations/day. Requirements: <2s response latency, 90%+ "
            "CSAT, data privacy for healthcare customers, and a 6-month time-to-market."
        ),
        "decision_type": "model_selection",
        "agent_a": {
            "id": "ml-engineer",
            "label": "ML Engineer",
            "confidence": 0.85,
            "outcome": (
                "Fine-tune Llama 3.1 70B on our domain corpus and self-host on 2× A100 "
                "instances. Projects 94% accuracy on our eval set, eliminates per-token "
                "API costs (~$180K/year savings), and keeps PHI on-prem for HIPAA."
            ),
            "reasoning": textwrap.dedent("""\
                For healthcare customers, data residency is non-negotiable. Any GPT-4o
                call sends PHI to a third-party API — a BAA doesn't eliminate the
                compliance risk, it just shifts liability.

                Key factors:
                1. Accuracy: Llama 3.1 70B fine-tuned on 80K in-domain support
                   transcripts scores 94% on our eval set vs 89% for GPT-4o zero-shot.
                2. Cost: at 50K conversations/day (~800 tokens avg), GPT-4o costs
                   $180K/year at current pricing. Self-hosting amortizes to ~$40K/year
                   after instance costs.
                3. Latency: P99 inference on A100 is 1.4s — within our 2s SLA.
                4. Control: we own the weights. No vendor rate limits, no API outages
                   disrupting customer support during incident spikes.

                Operational cost: 3 months to build fine-tuning pipeline. Worth it.
            """),
            "role": "ML Engineer",
            "goal": "Build accurate, cost-efficient, privacy-preserving AI systems.",
            "backstory": (
                "You are an ML engineer with experience deploying LLMs at healthcare "
                "companies. You are deeply skeptical of third-party API dependencies "
                "for sensitive use cases and believe fine-tuned OSS models outperform "
                "general-purpose APIs in domain-specific tasks."
            ),
        },
        "agent_b": {
            "id": "product-manager",
            "label": "Product Manager",
            "confidence": 0.82,
            "outcome": (
                "Use GPT-4o via the OpenAI API with a HIPAA BAA. Ships in 2 weeks, "
                "no GPU infrastructure, and the model's instruction-following eliminates "
                "the fine-tuning pipeline the team doesn't have capacity to maintain."
            ),
            "reasoning": textwrap.dedent("""\
                The fine-tuning path trades a known risk (vendor API) for three hidden
                risks: delayed launch, model drift as support patterns change, and
                on-call burden for a GPU fleet.

                Key factors:
                1. Time to market: a 3-month fine-tuning pipeline means we miss the Q3
                   launch window. GPT-4o with a prompt template ships in 2 weeks.
                2. HIPAA: OpenAI offers a BAA. We've used it for three other products.
                   Compliance reviewed and signed off.
                3. Maintenance: fine-tuned models degrade as the product changes.
                   Retraining quarterly requires an ML platform the team doesn't have.
                4. Accuracy gap: 89% vs 94% sounds significant, but our current human
                   agent CSAT is 87%. GPT-4o already exceeds the baseline without
                   fine-tuning.

                Build the fine-tuning capability when we have scale and data to justify it.
            """),
            "role": "Product Manager",
            "goal": "Ship reliable AI products on time that customers actually trust.",
            "backstory": (
                "You are a product manager who has launched three AI features in the "
                "last two years. You've seen ML engineers underestimate fine-tuning "
                "maintenance costs and overestimate accuracy gains on real user traffic."
            ),
        },
    },

    # ── 2: Observability Database ─────────────────────────────────────────
    {
        "id": "observability_database",
        "description": (
            "Choose a database for the new observability platform that will ingest "
            "2TB/day of metrics, logs, and traces from 400 services. Requirements: "
            "sub-second dashboards, 13-month hot retention, and a 4-engineer ops team."
        ),
        "decision_type": "architecture",
        "agent_a": {
            "id": "data-engineer",
            "label": "Data Engineer",
            "confidence": 0.87,
            "outcome": (
                "Adopt ClickHouse as the observability store. Columnar MergeTree engine "
                "handles 2TB/day ingest with P99 query latency under 200ms. Kafka to "
                "ClickHouse via native connector, Grafana for dashboards."
            ),
            "reasoning": textwrap.dedent("""\
                Observability data has a well-understood access pattern: append-only
                writes, range-scan aggregations, and high-cardinality filtering.
                ClickHouse is purpose-built for exactly this workload.

                Key factors:
                1. Ingest throughput: ClickHouse handles 1M+ rows/sec on modest hardware.
                   At 2TB/day (~23MB/sec), we have ample headroom.
                2. Query performance: columnar storage with vectorized execution makes
                   "sum(errors) GROUP BY service, 5min bucket" a sub-100ms operation
                   even over 13 months of data.
                3. Compression: ZSTD compression reduces 2TB/day to ~400GB stored —
                   5× cheaper than row-oriented storage.
                4. Ecosystem: native Grafana plugin, Kafka consumer, and S3-backed cold
                   storage. The integration stack requires no custom glue code.

                Tradeoff: 4 weeks of team ramp-up. Worth it for 5× compression and
                5× query performance at our retention window.
            """),
            "role": "Data Engineer",
            "goal": "Build data pipelines and stores that are fast, cheap, and reliable at scale.",
            "backstory": (
                "You are a data engineer who has designed observability platforms at "
                "two infrastructure companies. You believe columnar databases are the "
                "only rational choice for time-series analytics at this scale."
            ),
        },
        "agent_b": {
            "id": "sre-lead",
            "label": "SRE Lead",
            "confidence": 0.83,
            "outcome": (
                "Use TimescaleDB (PostgreSQL + TimescaleDB extension). The team already "
                "operates PostgreSQL — hypertables, continuous aggregates, and compression "
                "cover our requirements. No new database paradigm, no new on-call runbook."
            ),
            "reasoning": textwrap.dedent("""\
                Every database you add to your stack is a new thing that pages you at 3am.
                We already operate PostgreSQL with expertise, runbooks, and tooling.
                TimescaleDB extends it with exactly what we need.

                Key factors:
                1. Operational cost: our team runs 12 PostgreSQL clusters. TimescaleDB
                   is a Postgres extension — same backup strategy, same failover, same
                   monitoring. Zero new operational surface area.
                2. Continuous aggregates: pre-materialized rollup views update
                   automatically. Dashboard queries hit pre-computed results, not raw
                   data. P99 under 500ms without ClickHouse's complexity.
                3. SQL compatibility: every engineer already knows Postgres SQL.
                   ClickHouse's dialect has subtle differences that produce incorrect
                   results if you're not careful (NULL semantics, date functions).
                4. Compression: TimescaleDB columnar compression achieves 10–20× on
                   time-series data. 2TB/day stored as 100–200GB — comparable to ClickHouse.

                Introducing ClickHouse for 200ms vs 500ms dashboards is not worth
                the operational risk for a 4-engineer team.
            """),
            "role": "SRE Lead",
            "goal": "Keep systems reliable, operable, and simple for a small engineering team.",
            "backstory": (
                "You are an SRE lead responsible for a fleet of 50 services. You have "
                "introduced two new databases in the past year and watched both become "
                "operational burdens. You believe operational simplicity beats theoretical "
                "performance advantages."
            ),
        },
    },

    # ── 3: Service-to-Service Authentication ─────────────────────────────
    {
        "id": "service_auth",
        "description": (
            "Define the service-to-service authentication strategy for a 40-service "
            "microservices platform. Requirements: zero-trust posture, full audit trail, "
            "and support for both internal services and third-party integrations."
        ),
        "decision_type": "security",
        "agent_a": {
            "id": "security-architect",
            "label": "Security Architect",
            "confidence": 0.91,
            "outcome": (
                "Require mutual TLS (mTLS) for all service-to-service calls using "
                "SPIFFE/SPIRE for identity and short-lived X.509 certs. Every connection "
                "is cryptographically authenticated; no ambient trust."
            ),
            "reasoning": textwrap.dedent("""\
                Zero-trust requires cryptographic identity at the transport layer.
                OAuth2 client credentials give you auth tokens — mTLS gives you
                verified identity on every packet.

                Key factors:
                1. Ephemeral identity: SPIFFE SVIDs rotate every hour. Compromise of
                   one service doesn't grant lateral access — the cert is short-lived
                   and scoped to that workload's SPIFFE ID.
                2. Zero ambient trust: mTLS refuses connections at the TCP handshake,
                   before a single byte of application data is sent. OAuth2 token theft
                   is possible; ephemeral cert theft is not practical at our scale.
                3. Audit: SPIRE logs every cert issuance. We get a cryptographic audit
                   trail of every workload identity event, not just token exchanges.
                4. Standards: SPIFFE is a CNCF standard with Envoy, Linkerd, and Istio
                   first-class support. This is not experimental.

                Cert rotation is fully automated — the operational burden argument
                is a myth when running SPIRE.
            """),
            "role": "Security Architect",
            "goal": "Design zero-trust security architectures that eliminate ambient trust.",
            "backstory": (
                "You are a security architect who has designed zero-trust networks for "
                "financial services firms. You believe any authentication that relies on "
                "a shared secret (API keys, client secrets) is fundamentally weaker than "
                "public-key cryptography with short-lived, workload-scoped certs."
            ),
        },
        "agent_b": {
            "id": "platform-engineer",
            "label": "Platform Engineer",
            "confidence": 0.84,
            "outcome": (
                "Use OAuth2 client credentials flow for service-to-service auth. Each "
                "service holds a client ID and secret, exchanges it for a short-lived JWT "
                "from our internal auth server. Compatible with existing API gateway and "
                "third-party integrations without cert distribution."
            ),
            "reasoning": textwrap.dedent("""\
                mTLS solves the wrong problem. Our threat model is application-layer
                compromise, not network-layer eavesdropping. OAuth2 addresses it better
                and doesn't add a distributed control plane we'd have to operate.

                Key factors:
                1. Third-party integration: 8 of our 40 services expose APIs to partners.
                   mTLS requires distributing client certs to third parties — a support
                   and renewal nightmare. OAuth2 client credentials is the industry
                   standard for B2B API auth.
                2. Gateway inspection: our API gateway can inspect and log JWT claims.
                   mTLS traffic is opaque at the application layer — we lose visibility
                   into which service called what endpoint.
                3. Operational reality: SPIRE requires a control plane the platform team
                   would own and on-call. OAuth2 auth server is a solved problem —
                   we already run Keycloak for user auth, extend it.
                4. Rotation: rotating a client secret is a config update and a restart.
                   Rotating certs across 40 services with a SPIRE control plane, if
                   automation fails, takes services down simultaneously.

                Adopt mTLS for internal-only services in year 2, once the team has the
                operational maturity. Ship OAuth2 now for all paths.
            """),
            "role": "Platform Engineer",
            "goal": "Build platform primitives that are secure, operable, and developer-friendly.",
            "backstory": (
                "You are a platform engineer who has operated authentication systems for "
                "microservices platforms. You believe security architectures must account "
                "for operational burden — a system too complex to operate safely is not "
                "actually more secure."
            ),
        },
    },
]


# ---------------------------------------------------------------------------
# Terminal styling
# ---------------------------------------------------------------------------

RESET = "\033[0m"
BOLD = "\033[1m"
DIM = "\033[2m"
RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
CYAN = "\033[36m"
WHITE = "\033[97m"


def _c(text: str, *codes: str) -> str:
    return "".join(codes) + text + RESET


def _hr(char: str = "─", width: int = 72) -> None:
    print(_c(char * width, DIM))


def _banner(scenario_id: str) -> None:
    print()
    _hr("═")
    print(_c("  AKASHI  ·  Multi-Agent Conflict Detection Demo", BOLD, CYAN))
    print(_c(f"  Scenario: {scenario_id}", DIM))
    _hr("═")
    print()


def _section(title: str) -> None:
    print()
    _hr()
    print(_c(f"  {title}", BOLD))
    _hr()


def _step(icon: str, msg: str, color: str = WHITE) -> None:
    print(f"  {icon}  {_c(msg, color)}")


def _agent_box(name: str, text: str, color: str = CYAN) -> None:
    width = 68
    print()
    top = f"  ┌─ {_c(name, BOLD, color)} " + "─" * (width - len(name) - 5) + "┐"
    print(top)
    for line in text.strip().splitlines():
        padded = line.ljust(width - 2)
        print(f"  │ {padded} │")
    print(f"  └{'─' * (width - 2)}┘")
    print()


def _outcome_box(name: str, outcome: str) -> None:
    lines = textwrap.wrap(outcome, width=64)
    print(f"  {_c('OUTCOME', BOLD, YELLOW)}  [{name}]")
    for line in lines:
        print(f"    {line}")
    print()


# ---------------------------------------------------------------------------
# Load config
# ---------------------------------------------------------------------------

def _load_admin_key() -> str:
    key = os.environ.get("AKASHI_ADMIN_API_KEY", "")
    if key:
        return key
    env_file = Path(__file__).resolve().parent.parent.parent / ".env"
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            line = line.strip()
            if line.startswith("AKASHI_ADMIN_API_KEY="):
                return line.split("=", 1)[1].strip()
    sys.exit(
        "ERROR: AKASHI_ADMIN_API_KEY not found.\n"
        "Set it in akashi/.env or export AKASHI_ADMIN_API_KEY=<key>"
    )


# ---------------------------------------------------------------------------
# Akashi client
# We bypass the SDK deliberately: the SDK ties agent_id to the authenticated
# identity, but the demo needs two distinct agent voices. Admin JWT allows
# passing any agent_id in the trace body.
# ---------------------------------------------------------------------------

class _AkashiDemo:
    def __init__(self, api_key: str) -> None:
        self._client = httpx.Client(timeout=30.0)
        self._jwt = self._authenticate(api_key)

    def _authenticate(self, api_key: str) -> str:
        resp = self._client.post(
            f"{AKASHI_URL}/auth/token",
            json={"agent_id": "admin", "api_key": api_key},
        )
        if resp.status_code != 200:
            sys.exit(
                f"Auth failed ({resp.status_code}): {resp.text[:200]}\n"
                "Is the Akashi server running at http://localhost:8080?"
            )
        return resp.json()["data"]["token"]

    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self._jwt}"}

    def trace(
        self,
        agent_id: str,
        decision_type: str,
        outcome: str,
        reasoning: str,
        confidence: float = 0.85,
    ) -> str:
        resp = self._client.post(
            f"{AKASHI_URL}/v1/trace",
            json={
                "agent_id": agent_id,
                "decision": {
                    "decision_type": decision_type,
                    "outcome": outcome,
                    "reasoning": reasoning,
                    "confidence": confidence,
                },
            },
            headers=self._headers(),
        )
        resp.raise_for_status()
        data = resp.json().get("data", resp.json())
        return data.get("decision_id", "unknown")

    def poll_conflicts(
        self,
        agent_ids: list[str],
        decision_type: str,
        timeout: int = 30,
        interval: float = 2.0,
    ) -> list[dict]:
        deadline = time.time() + timeout
        spinner = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
        tick = 0
        while time.time() < deadline:
            for agent_id in agent_ids:
                resp = self._client.get(
                    f"{AKASHI_URL}/v1/conflicts",
                    params={
                        "status": "open",
                        "agent_id": agent_id,
                        "decision_type": decision_type,
                        "limit": 10,
                    },
                    headers=self._headers(),
                )
                if resp.status_code == 200:
                    items = resp.json().get("data", [])
                    if items:
                        print()
                        return items
            tick += 1
            elapsed = int(deadline - time.time())
            print(
                f"  {spinner[tick % len(spinner)]}  Waiting for conflict detection..."
                f" ({elapsed}s remaining)",
                end="\r",
            )
            time.sleep(interval)
        print()
        return []

    def close(self) -> None:
        self._client.close()


# ---------------------------------------------------------------------------
# Live mode: actual CrewAI agents (requires OPENAI_API_KEY)
# ---------------------------------------------------------------------------

def _run_live_agent(
    role: str, goal: str, backstory: str, task_description: str
) -> tuple[str, str]:
    """Run a real CrewAI agent and return (outcome_summary, full_reasoning)."""
    try:
        from crewai import Agent, Task, Crew, Process  # type: ignore
    except ImportError:
        sys.exit("crewai is not installed. Run: pip install crewai")

    agent = Agent(
        role=role,
        goal=goal,
        backstory=backstory,
        verbose=False,
        allow_delegation=False,
    )
    task = Task(
        description=task_description,
        expected_output=(
            "A clear recommendation with your reasoning. "
            "Start with 'RECOMMENDATION:' followed by a one-sentence summary, "
            "then explain your rationale."
        ),
        agent=agent,
    )
    crew = Crew(agents=[agent], tasks=[task], process=Process.sequential, verbose=False)
    result = crew.kickoff()
    full_text = str(result)
    outcome = full_text
    if "RECOMMENDATION:" in full_text:
        after = full_text.split("RECOMMENDATION:", 1)[1].strip()
        outcome = after.split("\n")[0].strip()
    return outcome[:500], full_text[:1500]


# ---------------------------------------------------------------------------
# Main demo
# ---------------------------------------------------------------------------

def _parse_args() -> tuple[bool, dict]:
    live = "--live" in sys.argv

    scenario: dict | None = None
    if "--scenario" in sys.argv:
        try:
            idx = sys.argv.index("--scenario")
            n = int(sys.argv[idx + 1])
            scenario = SCENARIOS[n]
        except (ValueError, IndexError):
            sys.exit(
                f"ERROR: --scenario requires a number 0–{len(SCENARIOS) - 1}.\n"
                f"Available: {', '.join(str(i) + '=' + s['id'] for i, s in enumerate(SCENARIOS))}"
            )

    if scenario is None:
        scenario = random.choice(SCENARIOS)

    return live, scenario


def main() -> None:
    live, scenario = _parse_args()

    if live and not os.environ.get("OPENAI_API_KEY"):
        sys.exit(
            "ERROR: --live requires OPENAI_API_KEY to be set.\n"
            "Run without --live for the pre-scripted demo (no API key needed)."
        )

    agent_a = scenario["agent_a"]
    agent_b = scenario["agent_b"]
    decision_type = scenario["decision_type"]

    _banner(scenario["id"])

    # ── Auth ──────────────────────────────────────────────────────────────
    _step("🔑", "Authenticating with Akashi...")
    akashi = _AkashiDemo(_load_admin_key())
    _step("✓", f"Connected  →  {AKASHI_URL}", GREEN)

    # ── Scenario ──────────────────────────────────────────────────────────
    _section("THE SCENARIO")
    print()
    for line in textwrap.wrap(scenario["description"], width=66):
        print(f"  {line}")

    if live:
        print()
        _step("⚡", "Live mode: running real CrewAI agents (this takes ~30s)", YELLOW)

    # ── Agent A ───────────────────────────────────────────────────────────
    _section(f"AGENT 1  ·  {agent_a['id']}")
    print()

    if live:
        _step("🤔", f"Analyzing as {agent_a['role']} with LLM...")
        outcome_a, reasoning_a = _run_live_agent(
            role=agent_a["role"],
            goal=agent_a["goal"],
            backstory=agent_a["backstory"],
            task_description=scenario["description"],
        )
    else:
        _step("🤔", "Analyzing requirements...")
        time.sleep(1.2)
        outcome_a = agent_a["outcome"]
        reasoning_a = agent_a["reasoning"]

    _agent_box(agent_a["id"], reasoning_a, CYAN)
    _outcome_box(agent_a["id"], outcome_a)

    _step("📡", "Tracing decision to Akashi...")
    decision_a = akashi.trace(
        agent_id=agent_a["id"],
        decision_type=decision_type,
        outcome=outcome_a,
        reasoning=reasoning_a.strip(),
        confidence=agent_a["confidence"],
    )
    _step("✓", f"Recorded  decision_id={decision_a}", GREEN)

    # Give the outbox worker time to sync A to Qdrant before B is traced.
    # Conflict detection queries Qdrant for similar decisions; A must be indexed.
    # Outbox polls every 1s; 6s allows for batch processing and cold-start.
    time.sleep(6.0)

    # ── Agent B ───────────────────────────────────────────────────────────
    _section(f"AGENT 2  ·  {agent_b['id']}")
    print()

    if live:
        _step("🤔", f"Analyzing as {agent_b['role']} with LLM (independent context)...")
        outcome_b, reasoning_b = _run_live_agent(
            role=agent_b["role"],
            goal=agent_b["goal"],
            backstory=agent_b["backstory"],
            task_description=scenario["description"],
        )
    else:
        _step("🤔", "Analyzing requirements (independent context)...")
        time.sleep(1.2)
        outcome_b = agent_b["outcome"]
        reasoning_b = agent_b["reasoning"]

    _agent_box(agent_b["id"], reasoning_b, YELLOW)
    _outcome_box(agent_b["id"], outcome_b)

    _step("📡", "Tracing decision to Akashi...")
    decision_b = akashi.trace(
        agent_id=agent_b["id"],
        decision_type=decision_type,
        outcome=outcome_b,
        reasoning=reasoning_b.strip(),
        confidence=agent_b["confidence"],
    )
    _step("✓", f"Recorded  decision_id={decision_b}", GREEN)

    # ── Conflict Detection ────────────────────────────────────────────────
    _section("AKASHI  ·  Conflict Detection Pipeline")
    print()
    _step("🔍", "Embedding decisions → Qdrant ANN search → top-50 candidates")
    _step("📊", "Claim-level scoring (topic similarity ≥ 0.7 threshold)...")
    _step("🧠", "LLM validator classifying relationship (contradiction / supersession)...")
    print()

    conflicts = akashi.poll_conflicts(
        agent_ids=[agent_a["id"], agent_b["id"]],
        decision_type=decision_type,
        timeout=30,
        interval=2.0,
    )

    if conflicts:
        c = conflicts[0]
        ca_id = c.get("agent_a", agent_a["id"])
        cb_id = c.get("agent_b", agent_b["id"])
        outcome_ca = c.get("outcome_a", outcome_a)
        outcome_cb = c.get("outcome_b", outcome_b)
        similarity = c.get("topic_similarity")
        divergence = c.get("outcome_divergence")
        explanation = c.get("explanation")

        print()
        _hr("═")
        print(_c("  ⚠   CONFLICT DETECTED", BOLD, RED))
        _hr("═")
        print()

        if similarity is not None:
            _step("📈", f"Topic similarity:   {similarity:.2f}  (same problem domain)")
        if divergence is not None:
            _step("📉", f"Outcome divergence: {divergence:.2f}  (opposing solutions)")
        print()

        print(_c(f"  [{ca_id}]", BOLD, CYAN))
        for line in textwrap.wrap(outcome_ca, width=64):
            print(f"    {line}")
        print()

        print(_c(f"  [{cb_id}]", BOLD, YELLOW))
        for line in textwrap.wrap(outcome_cb, width=64):
            print(f"    {line}")
        print()

        if explanation:
            _hr()
            print(_c("  LLM Validator", BOLD))
            for line in textwrap.wrap(explanation, width=66):
                print(f"  {line}")
            print()

        _hr("═")
        print()
        print(_c("  Two agents reached irreconcilable conclusions.", BOLD))
        print(_c("  Akashi flagged it automatically — no human had to compare notes.", DIM))
        print()
        print(f"  Resolve in the dashboard: {_c('http://localhost:8080/conflicts', BOLD, CYAN)}")
        print()

    else:
        print()
        _step("ℹ", "No conflict detected after 30s poll.", YELLOW)
        print()
        print("  Both decisions were recorded. Check the dashboard:")
        print(f"  → {_c('http://localhost:8080/decisions', BOLD, CYAN)}")
        print()
        print(_c("  Why no conflict? Common causes:", BOLD))
        print(_c("  • Embeddings disabled: need QDRANT_URL + OLLAMA or OPENAI_API_KEY", DIM))
        print(_c("  • Live mode: LLM outputs may be too similar (low outcome divergence)", DIM))
        print(_c("  • LLM validator: with OpenAI/Ollama, classifier may say 'complementary'", DIM))
        print(_c("  • Try pre-scripted mode: python demo.py --scenario 2  (no --live)", DIM))
        print()

    akashi.close()


if __name__ == "__main__":
    main()
