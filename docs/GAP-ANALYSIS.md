# Gap Analysis: Strategy vs. Implementation

## Executive Summary

The core technical infrastructure is solid. The gaps are primarily in **go-to-market enablement**, **self-serve experience**, and **moat-building mechanisms**. We have a black box recorder; we don't yet have the business wrapped around it.

---

## What We Accomplished ✅

### Core "Black Box Recorder" Functionality

| Capability | Status | Evidence |
|------------|--------|----------|
| Decision recording with full context | ✅ Complete | `akashi_trace` captures type, outcome, confidence, reasoning, alternatives, evidence |
| Bi-temporal audit trail | ✅ Complete | `valid_from`/`valid_to` + `transaction_time` in schema |
| Point-in-time queries | ✅ Complete | `POST /v1/query/temporal` with `as_of` parameter |
| Semantic search over decisions | ✅ Complete | pgvector HNSW index, `POST /v1/search` |
| Conflict detection | ✅ Complete | Materialized view refreshes every 30s |
| Immutable history | ✅ Complete | Revisions close old rows, insert new ones |

**Verdict:** The "black box" core is production-ready.

### Agent Integration (The "5 Minute Setup")

| Capability | Status | Evidence |
|------------|--------|----------|
| MCP tools with rich descriptions | ✅ Complete | 5 tools in `internal/mcp/tools.go` |
| MCP prompts for guidance | ✅ Complete | 3 prompts in `internal/mcp/prompts.go` |
| Check-before-decide workflow | ✅ Complete | `akashi_check` tool + `before-decision` prompt |
| Python SDK with middleware | ✅ Complete | `sdk/python/` with `AkashiMiddleware` |
| TypeScript SDK with middleware | ✅ Complete | `sdk/typescript/` with `withAkashi()` |
| System prompt templates | ✅ Complete | `prompts/generic.md`, `prompts/python.md`, `prompts/typescript.md` |

**Verdict:** Technical integration story is strong. An agent can start recording in minutes.

### Infrastructure Quality

| Capability | Status | Evidence |
|------------|--------|----------|
| Production-grade auth | ✅ Complete | Ed25519 JWT + Argon2id API keys |
| Connection pooling | ✅ Complete | PgBouncer for queries, direct conn for NOTIFY |
| OTEL observability | ✅ Complete | Traces and metrics exported |
| Docker Compose stack | ✅ Complete | Full stack in `docker/docker-compose.yml` |
| Graceful shutdown | ✅ Complete | Drains buffers, closes connections |
| Test coverage | ✅ Complete | 41 integration tests against real Postgres |

**Verdict:** This is production-grade infrastructure, not a prototype.

---

## What's Missing for "Insurance Pricing" 🔴

### No Usage Metering

Can't price like insurance without knowing usage:
- [ ] Decision count per agent/organization
- [ ] Storage consumption tracking
- [ ] API call metering
- [ ] Retention period enforcement

**Gap severity:** HIGH. Can't implement tiered pricing without this.

### No Multi-Tenancy

Current model: single-tenant (one Akashi instance = one customer)

Needed for PLG:
- [ ] Organization/workspace abstraction
- [ ] Tenant isolation in database
- [ ] Per-tenant usage limits
- [ ] Per-tenant retention policies

**Gap severity:** HIGH. Can't do self-serve without multi-tenancy.

### No Billing Integration

- [ ] Stripe/payment processor integration
- [ ] Usage-based billing calculation
- [ ] Tier upgrade/downgrade flows
- [ ] Invoice generation

**Gap severity:** HIGH. Can't charge money without billing.

---

## What's Missing for "Startup PLG" 🔴

### No Self-Serve Onboarding

Current flow: Deploy your own instance, configure env vars, run migrations.

Needed for PLG:
- [ ] Signup page (email/password or OAuth)
- [ ] Automatic workspace provisioning
- [ ] API key generation in UI
- [ ] "Copy this config" for MCP setup
- [ ] First-decision celebration / value moment

**Gap severity:** HIGH. No self-serve = no PLG.

### No Dashboard / Audit View

Current: API-only. No way to *show* the black box contents to a human.

Needed for startups selling to enterprise:
- [ ] Decision timeline view
- [ ] Decision detail view (reasoning, alternatives, evidence)
- [ ] Export to PDF for auditors
- [ ] Shareable audit report links
- [ ] Search/filter UI

**Gap severity:** HIGH. Startups need to *show* their enterprise prospects the audit trail.

### No "Wow Moment" in Onboarding

Current: Record a decision → get a UUID back.

Needed:
- [ ] Instant visualization of recorded decision
- [ ] "Here's what you'd show an auditor" preview
- [ ] Semantic search demo ("try searching for...")
- [ ] Conflict detection example

**Gap severity:** MEDIUM. Conversion depends on value demonstration.

---

## What's Missing for "Enterprise Compliance" 🟡

### No ISO 42001 Mapping Document

Strategy says this is Month 3 priority. Need:
- [ ] PDF/webpage mapping each ISO 42001 control to Akashi capability
- [ ] "How Akashi addresses Article 11" explainer
- [ ] Audit report templates

**Gap severity:** MEDIUM. Can still sell without it, but it's a closer accelerant.

### No Long-Term Retention

Current: TimescaleDB compresses after 7 days, but no explicit retention policy.

Needed for compliance:
- [ ] Configurable retention periods (90 days, 1 year, 10 years)
- [ ] Retention policy enforcement
- [ ] Legal hold capability (prevent deletion)
- [ ] Compliance-certified storage (SOC 2, etc.)

**Gap severity:** MEDIUM. Enterprise deals will ask about this.

### No Data Export for Auditors

- [ ] Full decision history export (CSV, JSON)
- [ ] Cryptographic integrity proofs
- [ ] Chain-of-custody documentation
- [ ] Auditor access mode (read-only, time-limited)

**Gap severity:** MEDIUM. Auditors will need to take data with them.

---

## What's Missing for "Moat Building" 🟡

### Moat 1: Switching Costs (Data Gravity)

Current state: Data is exportable (no lock-in). That's good for trust, bad for moat.

Needed:
- [ ] Analytics layer that only works with historical data
- [ ] "Decision intelligence" features (patterns, anomalies, trends)
- [ ] Compliance reports that reference historical corpus

**Gap severity:** LOW for now. Focus on adoption first.

### Moat 2: Ecosystem Integration

Current state: SDKs exist but aren't in framework docs.

Needed:
- [ ] LangChain integration merged upstream
- [ ] CrewAI documentation mentions Akashi
- [ ] AutoGen example showing Akashi integration
- [ ] Presence in "AI agent best practices" guides

**Gap severity:** MEDIUM. This is distribution, not product.

### Moat 3: Regulatory Trust

Current state: No external validation.

Needed:
- [ ] Compliance consultancy relationships
- [ ] Case study from regulated deployment
- [ ] SOC 2 Type II certification (long-term)
- [ ] ISO 42001 auditor familiarity

**Gap severity:** MEDIUM. Takes time to build.

### Moat 4: Network Effects

Current state: Single-org only. No cross-org queries.

Needed (long-term):
- [ ] Federated decision queries
- [ ] Industry precedent databases
- [ ] Privacy-preserving similarity search across orgs

**Gap severity:** LOW. This is post-PMF.

---

## Priority Stack: What to Build Next

### P0: PLG Foundation (Blocks Revenue)

1. **Multi-tenancy** — Organization model, tenant isolation
2. **Self-serve signup** — Registration, workspace provisioning, API key generation
3. **Billing integration** — Stripe, usage metering, tier enforcement
4. **Minimal dashboard** — Decision list, decision detail, audit export

Without these, we can't charge startups money.

### P1: Value Demonstration (Blocks Conversion)

5. **Onboarding flow** — Guided first decision, instant visualization
6. **Audit report generation** — PDF export, shareable links
7. **Usage dashboard** — Decision count, agent activity, storage

Without these, free users won't convert.

### P2: Compliance Credibility (Blocks Enterprise)

8. **ISO 42001 mapping document** — PDF for GRC teams
9. **Retention policy configuration** — Per-workspace settings
10. **Auditor access mode** — Read-only, time-limited tokens

Without these, enterprise deals stall.

### P3: Ecosystem Distribution (Blocks Growth)

11. **LangChain upstream integration**
12. **CrewAI documentation**
13. **"AI Governance 101" content marketing**

Without these, growth depends on outbound.

---

## Honest Assessment

### What We Have
A technically excellent black box recorder with strong agent integration primitives. The core value prop is real and working.

### What We Don't Have
A business. No way to sign up, no way to pay, no way to show the audit trail to humans, no way to demonstrate value before asking for money.

### The Gap
We built the engine. We didn't build the car around it.

### Estimated Effort to Close P0 Gaps

| Gap | Complexity | Estimate |
|-----|------------|----------|
| Multi-tenancy | High | 2-3 weeks |
| Self-serve signup | Medium | 1 week |
| Billing integration | Medium | 1 week |
| Minimal dashboard | Medium | 2 weeks |
| **Total P0** | | **6-7 weeks** |

### What This Means

The "Phases A-D" implementation built the *infrastructure* for a black box recorder.

The next phase ("Phase E"?) builds the *product* around it:
- The signup flow
- The dashboard
- The billing
- The onboarding experience

That's when we have something we can sell.

---

## Recommendation

**Don't chase enterprise sales yet.** The product isn't ready to demo to compliance buyers.

**Do build the PLG foundation.** Get to a state where:
1. Developer signs up in 2 minutes
2. Records first decision in 5 minutes
3. Sees the audit trail immediately
4. Shows it to their enterprise prospect
5. Upgrades to paid when they close the deal

That's the "Vanta for AI" experience. Everything else (ISO docs, enterprise features, ecosystem integrations) can come after the PLG loop is working.
