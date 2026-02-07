#!/usr/bin/env python3
"""
Seed Akashi with realistic demo decision data.

Generates decisions across multiple agents and decision types with real
Ollama embeddings. Designed to be rerunnable — pass --clear to wipe
previous seed data first.

Usage:
    python3 scripts/seed_demo_data.py                  # 5000 decisions
    python3 scripts/seed_demo_data.py --count 500      # custom count
    python3 scripts/seed_demo_data.py --clear           # wipe seed data first
    python3 scripts/seed_demo_data.py --clear --count 0 # just wipe, don't reseed

Requires:
    pip install psycopg
    Ollama running locally with mxbai-embed-large pulled
"""

import argparse
import json
import os
import random
import sys
import time
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone

try:
    import psycopg
except ImportError:
    print("psycopg not installed. Run: pip install psycopg", file=sys.stderr)
    sys.exit(1)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

DATABASE_URL = os.environ.get(
    "DATABASE_URL",
    "postgres://tsdbadmin:y7umgb9h487bgckm@xmrlv6epap.uy8qsyc8jg.tsdb.cloud.timescale.com:39116/tsdb?sslmode=require",
)
OLLAMA_URL = os.environ.get("OLLAMA_URL", "http://localhost:11434/api/embeddings")
EMBED_MODEL = os.environ.get("EMBED_MODEL", "mxbai-embed-large")
EMBED_WORKERS = int(os.environ.get("EMBED_WORKERS", "8"))
SEED_MARKER = "seed:demo-data"

# The org_id for the default (pre-signup) org — uuid.Nil.
DEFAULT_ORG_ID = "00000000-0000-0000-0000-000000000000"

# ---------------------------------------------------------------------------
# Agents
# ---------------------------------------------------------------------------

AGENTS = [
    {"agent_id": "planner", "name": "Planning Agent"},
    {"agent_id": "coder", "name": "Coding Agent"},
    {"agent_id": "reviewer", "name": "Review Agent"},
    {"agent_id": "researcher", "name": "Research Agent"},
    {"agent_id": "admin", "name": "System Admin"},
]

# ---------------------------------------------------------------------------
# Decision templates — realistic scenarios across all decision types
# ---------------------------------------------------------------------------

TEMPLATES = {
    "architecture": [
        ("microservices_decomposition", "Decomposed {service} into {n} microservices with gRPC inter-service communication",
         "Monolith was hitting scaling limits at {load}. Service boundaries align with team ownership. gRPC chosen over REST for internal calls due to {reason}."),
        ("event_driven_architecture", "Adopted event-driven architecture with {broker} for {domain}",
         "Decoupled {producer} from {consumer} to handle {volume} events/sec. Event sourcing provides natural audit trail. Chose {broker} for {broker_reason}."),
        ("caching_strategy", "Implemented {cache_type} caching with {ttl} TTL for {endpoint}",
         "Response times improved from {before}ms to {after}ms. Cache invalidation via {invalidation}. Hit rate expected at {hit_rate}%."),
        ("database_selection", "Selected {db} for {use_case} storage layer",
         "{db} provides {feature} which is critical for {requirement}. Evaluated {alt1} and {alt2} but they lacked {missing_feature}."),
        ("api_design", "Designed {style} API for {service} with {auth} authentication",
         "{style} chosen for {api_reason}. Pagination via cursor-based approach for consistent ordering. Rate limiting at {rate} req/s per client."),
        ("monolith_first", "Keeping {component} as monolith, deferring microservice split",
         "Current team size ({team_size}) doesn't justify operational complexity of distributed system. Will revisit at {threshold} requests/day."),
    ],
    "model_selection": [
        ("llm_for_summarization", "Selected {model} for {task} with {context_len}k context",
         "Benchmarked against {alt_model} on {n} test cases. {model} achieved {score}% accuracy at ${cost}/1M tokens. Latency p95: {latency}ms."),
        ("embedding_model_choice", "Chose {model} ({dims}d) for semantic search embeddings",
         "Evaluated {n_models} models. {model} best balance of quality (MTEB: {mteb}) and inference speed ({speed}ms/embed). {dims}d fits our pgvector index."),
        ("fine_tuning_decision", "{decision} fine-tuning for {task}",
         "Base model achieves {base_acc}% on our eval set. Fine-tuning estimated to reach {ft_acc}% but costs ${cost} and {time} of engineering time. {rationale}"),
        ("vision_model", "Using {model} for {vision_task} with {resolution} input",
         "OCR accuracy: {ocr_acc}% on our document types. Processing time: {proc_time}s per page. Cost: ${vision_cost}/1K images."),
        ("agent_framework", "Selected {framework} for multi-agent orchestration",
         "{framework} provides {feature} out of the box. Compared to {alt}: better {advantage}, weaker {weakness}. Community size: {community} GitHub stars."),
    ],
    "data_source": [
        ("data_pipeline_source", "Ingesting {data_type} data from {source} via {method}",
         "Volume: {volume} records/day. Freshness requirement: {freshness}. Chose {method} over {alt_method} for {reason}. Schema validation via {validator}."),
        ("vector_store_selection", "Using {store} as vector database for {use_case}",
         "Evaluated {n_stores} options. {store} chosen for {store_reason}. Index type: {index_type}. Query latency at {n_vectors}M vectors: {query_ms}ms."),
        ("feature_store", "Adopted {store} as feature store for {ml_use_case}",
         "Online serving latency: {latency}ms p99. Offline batch computation via {batch}. Feature freshness: {freshness}. {n_features} features serving {n_models} models."),
        ("api_data_source", "Integrating {api} API for {data_type} enrichment",
         "Rate limit: {rate}/min. Reliability: {uptime}% SLA. Fallback: {fallback}. Cost: ${cost}/month for estimated {volume} calls."),
    ],
    "error_handling": [
        ("retry_strategy", "Implemented {strategy} retry with {max_retries} max attempts for {service}",
         "Transient failure rate: {failure_rate}%. {strategy} backoff prevents thundering herd. Circuit breaker opens at {threshold}% error rate over {window}s window."),
        ("fallback_mechanism", "Added {fallback_type} fallback for {dependency} failures",
         "When {dependency} is unavailable, {fallback_action}. Degraded mode serves {coverage}% of requests. Alert triggers at {alert_threshold} consecutive failures."),
        ("dead_letter_queue", "Routing failed {event_type} events to DLQ with {retention} retention",
         "Poisonous messages were blocking the {queue}. DLQ allows manual inspection and replay. Retention: {retention}. Alert on DLQ depth > {threshold}."),
        ("graceful_degradation", "Graceful degradation for {feature} when {dependency} is slow",
         "Timeout set to {timeout}ms. On timeout: {action}. Users see {ux_impact}. Metrics show {pct_affected}% of requests hit timeout in peak hours."),
    ],
    "feature_scope": [
        ("mvp_scope", "Scoped {feature} MVP to {scope} for {release}",
         "Full feature requires {full_effort}. MVP covers {coverage}% of user requests. Deferred: {deferred}. Ship date: {date}."),
        ("feature_prioritization", "Prioritized {feature_a} over {feature_b} for {quarter}",
         "{feature_a} impacts {users_a} users vs {users_b} for {feature_b}. Revenue impact: ${revenue}. Tech debt score: {debt}/10. Customer requests: {requests}."),
        ("deprecation", "Deprecating {feature} in favor of {replacement}",
         "Usage dropped to {usage}% of peak. Maintenance cost: {cost}h/sprint. Migration path: {migration}. Sunset date: {sunset_date}."),
        ("ab_test_decision", "{outcome} variant {variant} for {experiment}",
         "Variant {variant} showed {metric}: {improvement}% improvement (p={pvalue}). Sample size: {n} users over {days} days. Rolling out to {rollout}%."),
    ],
    "trade_off": [
        ("latency_vs_accuracy", "Accepted {latency}ms latency for {accuracy}% accuracy on {task}",
         "Faster model ({fast_model}) achieves {fast_acc}% in {fast_lat}ms. Chose slower model for {reason}. SLA requires < {sla}ms p99."),
        ("cost_vs_quality", "Chose {tier} tier at ${cost}/month for {service}",
         "Premium tier offers {premium_feature} but costs {multiplier}x more. Current {metric} is {current_val}, premium would give {premium_val}. ROI threshold: {roi_months} months."),
        ("consistency_vs_availability", "Chose {consistency} consistency for {data_type}",
         "CAP theorem trade-off: {consistency} consistency means {availability_impact}. Acceptable for {data_type} because {reason}. Failover time: {failover}s."),
        ("build_vs_buy", "{decision} {component}",
         "Build cost: {build_cost}h + {maintenance}h/quarter maintenance. Buy cost: ${buy_cost}/month. Break-even: {breakeven} months. Chose {decision} because {reason}."),
    ],
    "deployment": [
        ("scaling_strategy", "Configured {strategy} scaling for {service} ({min}-{max} instances)",
         "Metric: {metric}. Scale-up threshold: {up_threshold}. Scale-down: {down_threshold}. Cooldown: {cooldown}s. Peak load handled: {peak} req/s."),
        ("deployment_strategy", "Using {strategy} deployment for {service}",
         "{strategy} minimizes downtime. Rollback time: {rollback}s. Health check interval: {health}s. Traffic shift: {shift}. Previous strategy ({old_strategy}) caused {old_issue}."),
        ("infrastructure_choice", "Deploying {service} on {platform} in {region}",
         "Latency to primary users ({user_region}): {latency}ms. Cost: ${cost}/month. Compliance: {compliance}. Evaluated {alt_platform}: {alt_reason}."),
        ("container_orchestration", "Using {orchestrator} for {workload_type} workloads",
         "{orchestrator} chosen for {orch_reason}. Node pool: {nodes} x {instance_type}. Resource limits: {cpu} CPU, {memory} RAM per pod."),
    ],
    "security": [
        ("auth_mechanism", "Implemented {auth_type} for {service} API authentication",
         "Token lifetime: {lifetime}. Refresh mechanism: {refresh}. Key rotation: {rotation}. Evaluated {alt_auth}: rejected due to {rejection_reason}."),
        ("encryption_decision", "Using {encryption} encryption for {data_type}",
         "Compliance requirement: {compliance}. Key management via {kms}. Performance overhead: {overhead}%. Rotation schedule: {rotation}."),
        ("access_control", "Implemented {rbac_type} access control with {n_roles} roles",
         "Principle of least privilege. {n_roles} roles mapped to {n_perms} permissions. Audit logging: {audit}. Review cadence: {review}."),
        ("vulnerability_response", "{action} for {vuln_type} vulnerability in {component}",
         "CVSS score: {cvss}. Affected users: {affected}. Mitigation: {mitigation}. Patch applied within {response_time}. No evidence of exploitation."),
    ],
}

# Fill-in pools for template variables
FILLS = {
    "service": ["user-service", "payment-service", "notification-service", "search-service",
                 "analytics-pipeline", "recommendation-engine", "auth-gateway", "data-ingestion",
                 "content-delivery", "order-processing", "inventory-service", "billing-service"],
    "n": [3, 4, 5, 6, 7, 8],
    "load": ["10K req/s", "50K req/s", "100K concurrent users", "1M daily active users"],
    "reason": ["type safety", "lower latency", "better streaming support", "schema evolution",
               "team expertise", "ecosystem maturity", "operational simplicity", "cost efficiency"],
    "broker": ["Kafka", "RabbitMQ", "NATS", "Redis Streams", "AWS SQS", "Pulsar"],
    "domain": ["order processing", "user analytics", "real-time notifications", "data sync",
               "audit logging", "payment processing", "inventory updates"],
    "producer": ["order service", "user service", "payment processor", "data pipeline"],
    "consumer": ["analytics", "notifications", "billing", "search indexer", "audit trail"],
    "volume": ["10K", "50K", "100K", "500K", "1M"],
    "broker_reason": ["exactly-once semantics", "low latency", "simple operations", "cost"],
    "cache_type": ["Redis", "Memcached", "CDN edge", "in-process LRU", "distributed"],
    "ttl": ["30s", "5min", "15min", "1hr", "24hr"],
    "endpoint": ["/api/search", "/api/recommendations", "/api/products", "/api/user/profile"],
    "before": [450, 800, 1200, 2000, 3500],
    "after": [12, 25, 45, 80, 150],
    "invalidation": ["event-driven", "TTL-based", "write-through", "cache-aside"],
    "hit_rate": [85, 90, 92, 95, 98],
    "db": ["PostgreSQL", "MongoDB", "DynamoDB", "CockroachDB", "ClickHouse", "ScyllaDB"],
    "use_case": ["transactional", "analytics", "time-series", "document", "graph", "search"],
    "feature": ["ACID transactions", "horizontal scaling", "columnar storage", "document flexibility"],
    "requirement": ["audit compliance", "sub-ms reads", "complex queries", "schema flexibility"],
    "alt1": ["MySQL", "Cassandra", "Redis", "SQLite", "MariaDB"],
    "alt2": ["Firebase", "Supabase", "PlanetScale", "Neon", "TiDB"],
    "missing_feature": ["bi-temporal queries", "vector search", "row-level security", "JSON indexing"],
    "style": ["REST", "GraphQL", "gRPC", "WebSocket"],
    "auth": ["JWT", "OAuth2", "API key", "mTLS"],
    "api_reason": ["broad client compatibility", "type-safe schema", "real-time requirements"],
    "rate": [100, 500, 1000, 5000],
    "component": ["auth system", "billing engine", "search service", "admin dashboard"],
    "team_size": [3, 5, 8, 12],
    "threshold": ["50K", "100K", "500K"],
    "model": ["GPT-4o", "Claude 3.5 Sonnet", "Llama 3.1 70B", "Gemini 1.5 Pro", "Mistral Large",
              "Claude Opus 4", "GPT-4o mini", "Qwen 2.5 72B", "DeepSeek V3"],
    "task": ["summarization", "classification", "extraction", "code generation", "RAG",
             "sentiment analysis", "translation", "question answering"],
    "context_len": [8, 16, 32, 128, 200],
    "alt_model": ["GPT-4o mini", "Claude Haiku", "Llama 3.1 8B", "Mistral 7B"],
    "score": [87, 89, 91, 93, 95, 97],
    "cost": [0.50, 1.00, 2.50, 5.00, 10.00, 15.00],
    "latency": [120, 250, 500, 800, 1500, 2000],
    "dims": [384, 512, 768, 1024, 1536],
    "n_models": [3, 5, 7, 10],
    "mteb": [58.2, 61.5, 64.8, 67.3, 69.1],
    "speed": [5, 12, 25, 50, 80],
    "decision": ["Proceeding with", "Skipping", "Deferring"],
    "base_acc": [72, 78, 82, 85],
    "ft_acc": [88, 91, 93, 95],
    "time": ["2 weeks", "1 month", "6 weeks"],
    "rationale": ["ROI is clear given production volume.", "Marginal gain doesn't justify cost.",
                  "Will revisit after collecting more training data."],
    "vision_task": ["document OCR", "receipt parsing", "diagram understanding", "chart extraction"],
    "resolution": ["1024x1024", "2048x2048", "variable"],
    "ocr_acc": [92, 95, 97, 99],
    "proc_time": [0.3, 0.8, 1.5, 2.5],
    "vision_cost": [1.50, 3.00, 5.00, 10.00],
    "framework": ["LangGraph", "CrewAI", "AutoGen", "custom orchestrator"],
    "advantage": ["state management", "tool integration", "debugging", "flexibility"],
    "weakness": ["documentation", "performance", "community support", "extensibility"],
    "community": [5000, 12000, 25000, 45000],
    "data_type": ["user behavior", "transaction", "sensor", "log", "clickstream"],
    "source": ["Kafka topic", "S3 bucket", "REST API", "CDC stream", "webhook"],
    "method": ["streaming ingestion", "batch ETL", "CDC replication", "API polling"],
    "freshness": ["real-time", "< 1 minute", "< 5 minutes", "hourly", "daily"],
    "alt_method": ["batch processing", "direct DB query", "file transfer"],
    "validator": ["JSON Schema", "Avro", "Protobuf", "Great Expectations"],
    "store": ["pgvector", "Pinecone", "Weaviate", "Qdrant", "Milvus", "Chroma"],
    "n_stores": [4, 5, 6],
    "store_reason": ["PostgreSQL integration", "managed service", "filtering support", "cost"],
    "index_type": ["HNSW", "IVFFlat", "SCANN"],
    "n_vectors": [1, 5, 10, 50],
    "query_ms": [5, 12, 25, 50, 100],
    "ml_use_case": ["recommendation", "fraud detection", "churn prediction"],
    "batch": ["Spark", "dbt", "Airflow", "Dagster"],
    "n_features": [50, 100, 250, 500],
    "api": ["Clearbit", "Stripe", "Twilio", "SendGrid", "OpenAI", "Google Maps"],
    "uptime": [99.5, 99.9, 99.95, 99.99],
    "fallback": ["cached response", "graceful degradation", "secondary provider"],
    "strategy": ["exponential", "linear", "jittered exponential", "fibonacci"],
    "max_retries": [3, 5, 7, 10],
    "failure_rate": [0.1, 0.5, 1.0, 2.5, 5.0],
    "window": [30, 60, 120, 300],
    "fallback_type": ["cached", "static", "degraded", "secondary provider"],
    "dependency": ["payment gateway", "email service", "search index", "ML inference", "CDN"],
    "fallback_action": ["serve cached results", "queue for retry", "return partial response"],
    "coverage": [60, 75, 85, 90, 95],
    "alert_threshold": [3, 5, 10],
    "event_type": ["payment", "notification", "webhook", "sync"],
    "retention": ["7 days", "14 days", "30 days", "90 days"],
    "queue": ["main processing queue", "notification pipeline", "sync queue"],
    "timeout": [500, 1000, 2000, 3000, 5000],
    "action": ["return cached result", "skip enrichment", "use default value", "show loading state"],
    "ux_impact": ["slightly stale data", "reduced personalization", "generic recommendations"],
    "pct_affected": [2, 5, 8, 12],
    "feature_a": ["real-time search", "user dashboard", "API v2", "batch export", "SSO"],
    "feature_b": ["dark mode", "mobile app", "webhooks", "custom reports", "SAML"],
    "quarter": ["Q1 2026", "Q2 2026", "Q3 2026", "Q4 2026"],
    "users_a": [5000, 12000, 25000],
    "users_b": [500, 1500, 3000],
    "revenue": ["50K", "150K", "500K"],
    "debt": [3, 5, 7, 8],
    "requests": [45, 120, 300, 500],
    "feature": ["legacy export API", "v1 webhooks", "XML endpoint", "SOAP interface"],
    "replacement": ["v2 REST API", "GraphQL endpoint", "streaming API", "gRPC service"],
    "usage": [5, 12, 20],
    "migration": ["automated script", "manual with guide", "gradual rollout"],
    "sunset_date": ["2026-06-01", "2026-09-01", "2026-12-01"],
    "outcome": ["Shipped", "Rolled back", "Kept"],
    "variant": ["A", "B", "C"],
    "experiment": ["checkout flow", "pricing page", "onboarding", "search UI"],
    "metric": ["conversion rate", "engagement", "retention", "revenue per user"],
    "improvement": [3.2, 5.7, 8.1, 12.4, 15.0],
    "pvalue": [0.001, 0.005, 0.01, 0.03, 0.05],
    "days": [7, 14, 21, 30],
    "rollout": [25, 50, 75, 100],
    "fast_model": ["GPT-4o mini", "Claude Haiku", "Llama 8B"],
    "fast_acc": [78, 82, 85],
    "fast_lat": [50, 100, 150],
    "accuracy": [92, 95, 97],
    "sla": [500, 1000, 2000, 3000],
    "tier": ["standard", "professional", "enterprise"],
    "multiplier": [2, 3, 5, 10],
    "premium_feature": ["99.99% SLA", "dedicated support", "custom model", "priority queue"],
    "current_val": ["99.5%", "500ms p99", "10K req/s"],
    "premium_val": ["99.99%", "100ms p99", "100K req/s"],
    "roi_months": [3, 6, 12, 18],
    "consistency": ["strong", "eventual", "causal", "read-your-writes"],
    "availability_impact": ["failover downtime up to 30s", "stale reads for 1-5s", "limited write availability"],
    "failover": [5, 15, 30, 60],
    "build_cost": [80, 160, 320, 500],
    "maintenance": [20, 40, 80],
    "buy_cost": [200, 500, 1000, 2500],
    "breakeven": [6, 12, 18, 24],
    "min": [1, 2, 3],
    "max": [5, 10, 20, 50],
    "up_threshold": ["70% CPU", "80% CPU", "1000 req/s", "p95 > 500ms"],
    "down_threshold": ["30% CPU", "40% CPU", "200 req/s", "p95 < 100ms"],
    "cooldown": [60, 120, 300],
    "peak": [5000, 10000, 50000],
    "rollback": [10, 30, 60, 120],
    "health": [5, 10, 15, 30],
    "shift": ["10% increments", "canary (1%)", "50/50 split", "all-at-once"],
    "old_strategy": ["big-bang", "manual", "blue-green"],
    "old_issue": ["20min downtime", "configuration drift", "resource waste"],
    "platform": ["AWS ECS", "GCP Cloud Run", "Fly.io", "Railway", "Kubernetes"],
    "region": ["us-east-1", "eu-west-1", "ap-southeast-1", "us-west-2"],
    "user_region": ["US East", "Europe", "Asia Pacific"],
    "compliance": ["SOC 2", "GDPR", "HIPAA", "ISO 27001"],
    "alt_platform": ["Heroku", "DigitalOcean", "Render", "Vercel"],
    "alt_reason": ["higher cost", "less control", "region limitations", "scaling limits"],
    "orchestrator": ["Kubernetes", "ECS", "Nomad", "Docker Swarm"],
    "workload_type": ["stateless API", "batch processing", "ML inference", "data pipeline"],
    "orch_reason": ["ecosystem", "team expertise", "cost", "managed service"],
    "nodes": [3, 5, 8, 12, 20],
    "instance_type": ["m6i.xlarge", "c6i.2xlarge", "r6i.xlarge", "t3.large"],
    "cpu": ["500m", "1", "2", "4"],
    "memory": ["512Mi", "1Gi", "2Gi", "4Gi"],
    "auth_type": ["Ed25519 JWT", "OAuth 2.0 + PKCE", "API key + HMAC", "mTLS"],
    "lifetime": ["15 minutes", "1 hour", "24 hours"],
    "refresh": ["rotating refresh tokens", "sliding session", "re-authentication"],
    "rotation": ["every 90 days", "every 30 days", "automatic on compromise"],
    "alt_auth": ["session cookies", "basic auth", "SAML", "API key only"],
    "rejection_reason": ["no revocation support", "CSRF vulnerability", "browser-only", "no granularity"],
    "encryption": ["AES-256-GCM", "ChaCha20-Poly1305", "RSA-OAEP"],
    "kms": ["AWS KMS", "HashiCorp Vault", "GCP KMS", "Azure Key Vault"],
    "overhead": [1, 3, 5, 8],
    "rbac_type": ["hierarchical RBAC", "ABAC", "ReBAC"],
    "n_roles": [4, 5, 6, 8],
    "n_perms": [12, 20, 30, 50],
    "audit": ["all access logged", "write operations only", "sensitive resources only"],
    "review": ["quarterly", "monthly", "on role change"],
    "vuln_type": ["SQL injection", "XSS", "SSRF", "dependency CVE", "auth bypass"],
    "cvss": [4.3, 5.5, 7.2, 8.1, 9.8],
    "affected": ["all users", "admin users only", "API consumers", "internal services"],
    "mitigation": ["input validation", "dependency upgrade", "WAF rule", "config change"],
    "response_time": ["2 hours", "4 hours", "24 hours", "48 hours"],
}


def fill_template(template: str) -> str:
    """Replace {placeholders} in a template with random values from FILLS."""
    result = template
    # Find all {key} patterns and replace them
    import re
    for match in re.finditer(r'\{(\w+)\}', template):
        key = match.group(1)
        if key in FILLS:
            val = random.choice(FILLS[key])
            result = result.replace(match.group(0), str(val), 1)
    return result


def generate_decisions(count: int) -> list[dict]:
    """Generate `count` realistic decision records."""
    decisions = []
    decision_types = list(TEMPLATES.keys())
    now = datetime.now(timezone.utc)

    for i in range(count):
        dtype = random.choice(decision_types)
        templates = TEMPLATES[dtype]
        outcome_tpl, outcome_detail_tpl, reasoning_tpl = random.choice(templates)

        outcome = fill_template(outcome_detail_tpl)
        reasoning = fill_template(reasoning_tpl)
        agent = random.choice(AGENTS)
        confidence = round(random.uniform(0.4, 1.0), 2)

        # Spread decisions over the last 90 days
        days_ago = random.uniform(0, 90)
        created = now - timedelta(days=days_ago, seconds=random.randint(0, 86400))

        decisions.append({
            "id": str(uuid.uuid4()),
            "agent_id": agent["agent_id"],
            "decision_type": dtype,
            "outcome": outcome,
            "reasoning": reasoning,
            "confidence": confidence,
            "created_at": created,
            "embed_text": f"{dtype}: {outcome} {reasoning}",
        })

    return decisions


# ---------------------------------------------------------------------------
# Ollama embedding with parallelism
# ---------------------------------------------------------------------------

def embed_one(text: str) -> list[float]:
    """Get embedding from Ollama for a single text."""
    payload = json.dumps({"model": EMBED_MODEL, "prompt": text}).encode()
    req = urllib.request.Request(OLLAMA_URL, data=payload, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read())
    return data["embedding"]


def embed_batch(decisions: list[dict]) -> list[dict]:
    """Generate embeddings for all decisions using a thread pool."""
    total = len(decisions)
    completed = 0
    start = time.time()

    def process(idx: int, dec: dict) -> tuple[int, list[float]]:
        return idx, embed_one(dec["embed_text"])

    with ThreadPoolExecutor(max_workers=EMBED_WORKERS) as pool:
        futures = {pool.submit(process, i, d): i for i, d in enumerate(decisions)}
        for future in as_completed(futures):
            idx, embedding = future.result()
            decisions[idx]["embedding"] = embedding
            completed += 1
            if completed % 100 == 0 or completed == total:
                elapsed = time.time() - start
                rate = completed / elapsed
                eta = (total - completed) / rate if rate > 0 else 0
                print(f"  Embedded {completed}/{total} ({rate:.0f}/s, ETA {eta:.0f}s)")

    return decisions


# ---------------------------------------------------------------------------
# Database operations
# ---------------------------------------------------------------------------

def clear_seed_data(conn):
    """Remove previously seeded demo data."""
    print("Clearing previous seed data...")
    with conn.cursor() as cur:
        # Delete decisions, alternatives, evidence linked to seed runs
        cur.execute("""
            DELETE FROM alternatives WHERE decision_id IN (
                SELECT id FROM decisions WHERE metadata->>'seed_marker' = %s
            )
        """, (SEED_MARKER,))
        alt_count = cur.rowcount

        cur.execute("""
            DELETE FROM evidence WHERE decision_id IN (
                SELECT id FROM decisions WHERE metadata->>'seed_marker' = %s
            )
        """, (SEED_MARKER,))
        ev_count = cur.rowcount

        cur.execute("DELETE FROM decisions WHERE metadata->>'seed_marker' = %s", (SEED_MARKER,))
        dec_count = cur.rowcount

        cur.execute("DELETE FROM agent_events WHERE run_id IN (SELECT id FROM agent_runs WHERE metadata->>'seed_marker' = %s)", (SEED_MARKER,))
        evt_count = cur.rowcount

        cur.execute("DELETE FROM agent_runs WHERE metadata->>'seed_marker' = %s", (SEED_MARKER,))
        run_count = cur.rowcount

    conn.commit()
    print(f"  Cleared {run_count} runs, {dec_count} decisions, {alt_count} alternatives, {ev_count} evidence, {evt_count} events")


def insert_data(conn, decisions: list[dict]):
    """Insert runs + decisions + alternatives into the database."""
    print(f"Inserting {len(decisions)} decisions into database...")

    # Group decisions into runs (1-4 decisions per run)
    runs = []
    run_decisions = {}
    i = 0
    while i < len(decisions):
        run_size = random.randint(1, 4)
        run_id = str(uuid.uuid4())
        agent_id = decisions[i]["agent_id"]
        created_at = decisions[i]["created_at"]

        runs.append({
            "id": run_id,
            "agent_id": agent_id,
            "org_id": DEFAULT_ORG_ID,
            "status": "completed",
            "created_at": created_at,
        })
        run_decisions[run_id] = []

        for j in range(run_size):
            if i + j >= len(decisions):
                break
            decisions[i + j]["run_id"] = run_id
            run_decisions[run_id].append(decisions[i + j])

        i += run_size

    # Generate alternatives for each decision
    alternative_labels = [
        "Use existing solution", "Build custom", "Adopt open-source", "Buy SaaS",
        "Defer decision", "Hybrid approach", "Minimal implementation", "Full rewrite",
        "Gradual migration", "Big bang switch", "Use managed service", "Self-host",
        "Increase budget", "Reduce scope", "Parallelize work", "Sequential rollout",
    ]

    alternatives = []
    for dec in decisions:
        n_alts = random.randint(1, 4)
        labels = random.sample(alternative_labels, min(n_alts, len(alternative_labels)))
        selected_idx = random.randint(0, n_alts - 1)
        for k, label in enumerate(labels):
            is_selected = (k == selected_idx)
            alternatives.append({
                "id": str(uuid.uuid4()),
                "decision_id": dec["id"],
                "label": label,
                "score": round(random.uniform(0.2, 1.0), 2) if random.random() > 0.3 else None,
                "selected": is_selected,
                "rejection_reason": None if is_selected else random.choice([
                    "Too expensive", "Too complex", "Insufficient features",
                    "Team lacks expertise", "Timeline too long", "Vendor lock-in risk",
                    "Doesn't meet compliance requirements", "Poor community support",
                    None,  # Sometimes no reason given
                ]),
            })

    with conn.cursor() as cur:
        # Insert runs
        print(f"  Inserting {len(runs)} runs...")
        for run in runs:
            cur.execute("""
                INSERT INTO agent_runs (id, agent_id, org_id, status, metadata, started_at, created_at)
                VALUES (%s, %s, %s, %s, %s, %s, %s)
                ON CONFLICT (id) DO NOTHING
            """, (
                run["id"], run["agent_id"], run["org_id"], run["status"],
                json.dumps({"seed_marker": SEED_MARKER}),
                run["created_at"], run["created_at"],
            ))

        # Insert decisions
        print(f"  Inserting {len(decisions)} decisions...")
        for dec in decisions:
            vec_str = "[" + ",".join(str(v) for v in dec["embedding"]) + "]"
            cur.execute("""
                INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome,
                    confidence, reasoning, embedding, quality_score, metadata,
                    valid_from, transaction_time, created_at)
                VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s::vector, %s, %s, %s, %s, %s)
                ON CONFLICT (id) DO NOTHING
            """, (
                dec["id"], dec["run_id"], dec["agent_id"], DEFAULT_ORG_ID,
                dec["decision_type"], dec["outcome"], dec["confidence"],
                dec["reasoning"], vec_str,
                round(random.uniform(0.3, 1.0), 2),
                json.dumps({"seed_marker": SEED_MARKER}),
                dec["created_at"], dec["created_at"], dec["created_at"],
            ))

        # Insert alternatives
        print(f"  Inserting {len(alternatives)} alternatives...")
        for alt in alternatives:
            cur.execute("""
                INSERT INTO alternatives (id, decision_id, label, score, selected, rejection_reason)
                VALUES (%s, %s, %s, %s, %s, %s)
                ON CONFLICT (id) DO NOTHING
            """, (
                alt["id"], alt["decision_id"], alt["label"],
                alt["score"], alt["selected"], alt["rejection_reason"],
            ))

    conn.commit()
    print(f"  Done! {len(runs)} runs, {len(decisions)} decisions, {len(alternatives)} alternatives")


def refresh_conflict_view(conn):
    """Refresh the materialized view so conflicts are detected."""
    print("Refreshing conflict materialized view...")
    with conn.cursor() as cur:
        cur.execute("REFRESH MATERIALIZED VIEW CONCURRENTLY decision_conflicts")
    conn.commit()
    print("  Done!")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="Seed Akashi with demo decision data")
    parser.add_argument("--count", type=int, default=5000, help="Number of decisions to generate (default: 5000)")
    parser.add_argument("--clear", action="store_true", help="Clear previous seed data before inserting")
    parser.add_argument("--no-refresh", action="store_true", help="Skip conflict materialized view refresh")
    args = parser.parse_args()

    conn = psycopg.connect(DATABASE_URL)

    if args.clear:
        clear_seed_data(conn)

    if args.count <= 0:
        print("Count is 0, nothing to seed.")
        conn.close()
        return

    print(f"\n=== Seeding {args.count} decisions ===\n")

    # Step 1: Generate decision data
    print("Step 1: Generating decision data...")
    t0 = time.time()
    decisions = generate_decisions(args.count)
    print(f"  Generated {len(decisions)} decisions in {time.time() - t0:.1f}s\n")

    # Step 2: Generate embeddings via Ollama
    print(f"Step 2: Generating embeddings ({EMBED_WORKERS} workers)...")
    t1 = time.time()
    decisions = embed_batch(decisions)
    embed_time = time.time() - t1
    print(f"  Embedded {len(decisions)} decisions in {embed_time:.1f}s ({len(decisions)/embed_time:.0f}/s)\n")

    # Step 3: Insert into database
    print("Step 3: Inserting into database...")
    t2 = time.time()
    insert_data(conn, decisions)
    print(f"  Inserted in {time.time() - t2:.1f}s\n")

    # Step 4: Refresh conflict view
    if not args.no_refresh:
        t3 = time.time()
        refresh_conflict_view(conn)
        print(f"  Refreshed in {time.time() - t3:.1f}s\n")

    conn.close()

    total_time = time.time() - t0
    print(f"=== Complete! {args.count} decisions seeded in {total_time:.0f}s ===")


if __name__ == "__main__":
    main()
