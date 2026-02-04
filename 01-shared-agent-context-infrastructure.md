# Kyoyu.ai: Shared Agent Context Infrastructure

## Company Spec v1.0 | January 2026

---

## Executive Summary

**Kyoyu.ai** builds the shared context layer for AI agents. As enterprises deploy fleets of specialized AI agents that must coordinate, hand off tasks, and collaborate with humans, the critical missing infrastructure is a persistent, queryable record of agent reasoning, decisions, and state.

We are the "git for agent decisions" - providing versioned, traceable, shareable context that flows between agents, systems, and humans.

**The Opportunity**: The AI agents market is growing from $7.8B (2025) to $52B+ by 2030 at 46% CAGR. [Gartner reports](https://www.gartner.com/en/articles/multiagent-systems) a 1,445% surge in multi-agent system inquiries from Q1 2024 to Q2 2025. Yet the infrastructure for agents to share context is primitive or non-existent.

**The Wedge**: LangChain ($1.25B valuation) and CrewAI ($18M raised) solve agent orchestration. We solve what happens *between* agents - the shared memory, decision traces, and handoff protocols that make multi-agent systems actually work in production.

---

## 1. Problem Statement

### The Context Crisis in Multi-Agent Systems

Modern AI agents are fundamentally stateless. LLMs don't retain information between calls. When enterprises deploy multiple specialized agents that must work together, three critical problems emerge:

#### Problem 1: Context Evaporation
> "A single user request might trigger 15+ LLM calls across multiple chains, models, and tools, creating execution paths that span embedding generation, vector retrieval, context assembly, multiple reasoning steps, and final response generation." - [Maxim AI Research](https://www.getmaxim.ai/articles/context-window-management-strategies-for-long-context-ai-agents-and-chatbots/)

When Agent A hands off to Agent B, context is lost. When a human needs to review an agent's decision, the reasoning is gone. When the same task runs again, past learnings aren't available.

#### Problem 2: Decision Opacity
> "The key missing layer in enterprises is decision traces - the exceptions, overrides, precedents, and cross-system context that currently live in Slack threads, deal desk conversations, escalation calls, and people's heads." - [Foundation Capital](https://foundationcapital.com/context-graphs-ais-trillion-dollar-opportunity/)

Enterprises deploying agents for consequential decisions (financial, medical, legal) cannot answer basic questions:
- Why did the agent make this decision?
- What information did it consider?
- Has this situation come up before? What happened?

#### Problem 3: Coordination Failure
Multi-agent systems require agents to:
- Share discoveries in real-time
- Avoid duplicating work
- Build on each other's reasoning
- Hand off gracefully to humans

Current solutions (shared variables, message passing) are primitive and don't scale.

### Current Solutions Are Inadequate

| Approach | Limitation |
|----------|------------|
| **Context windows** | Ephemeral, expensive, limited size |
| **Vector databases** | Good for retrieval, bad for structured reasoning traces |
| **Message queues** | Move data, don't preserve reasoning |
| **Custom state management** | Fragile, non-standard, maintenance burden |
| **LangGraph/CrewAI state** | Workflow-scoped, not shared across systems |

---

## 2. Market Opportunity

### Total Addressable Market (TAM)

The AI agents market is projected to grow from **$7.8B in 2025 to $52B+ by 2030** at 46% CAGR. [Grand View Research](https://www.grandviewresearch.com/industry-analysis/ai-agents-market-report) and [Markets and Markets](https://www.marketsandmarkets.com/Market-Reports/ai-agents-market-15761548.html) confirm this trajectory.

Within this, the **infrastructure layer** (context, observability, orchestration) represents approximately 15-20% of spend, or **$8-10B by 2030**.

### Serviceable Addressable Market (SAM)

Enterprises deploying production multi-agent systems requiring:
- Shared context between agents
- Decision audit trails
- Human-agent handoff protocols

**SAM**: $2-3B by 2030

### Serviceable Obtainable Market (SOM)

Year 1-3 target: Mid-market and enterprise companies with:
- 5+ agents in production
- Compliance/audit requirements
- Multi-team agent development

**SOM**: $100-300M by 2028

### Market Timing

**Why Now?**

1. **Agent proliferation**: 40% of enterprise apps will embed agents by end of 2026, up from <5% in 2025 ([Gartner](https://www.gartner.com/en/articles/multiagent-systems))

2. **Multi-agent adoption**: 1,445% surge in multi-agent inquiries Q1 2024 → Q2 2025

3. **Production pressure**: "2026 is when startups catch up to the ambition, and enterprises move from pilots to production" ([Foundation Capital](https://foundationcapital.com/where-ai-is-headed-in-2026/))

4. **Standards emerging**: OpenTelemetry is actively defining [AI agent semantic conventions](https://opentelemetry.io/blog/2025/ai-agent-observability/)

---

## 3. Solution: The Kyoyu Context Layer

### Product Vision

Kyoyu.ai provides **shared, persistent, queryable context** for AI agent systems. Every agent decision, reasoning step, tool call, and human intervention becomes part of a versioned context graph that any authorized agent or human can access.

### Core Capabilities

#### 3.1 Context Graph
A structured, versioned record of all agent activity:
- **Decision nodes**: What was decided and why
- **Evidence nodes**: What information was considered
- **Action nodes**: What was done
- **Handoff edges**: How context flowed between agents/humans

#### 3.2 Shared Memory Blocks
Following [Letta's memory block architecture](https://www.letta.com/blog/memory-blocks):
- Discrete, functional memory units
- Multiple agents can read/write
- Scoped access controls
- Automatic summarization and compression

#### 3.3 Decision Trace API
```python
# Record agent reasoning
kyoyu.trace(
    agent_id="underwriting-agent",
    decision="approve_loan",
    confidence=0.87,
    evidence=[doc_1, doc_2, credit_score],
    reasoning="Debt-to-income within threshold...",
    precedents=kyoyu.query("similar loan decisions")
)

# Query past decisions
similar = kyoyu.query(
    "loan approvals where DTI > 40%",
    time_range="last_90_days",
    outcome_filter="default"
)
```

#### 3.4 Handoff Protocol
Structured context transfer between:
- Agent → Agent
- Agent → Human
- Human → Agent
- Sync → Async workflows

#### 3.5 Context Replay
Re-run any decision with the exact context that was available at decision time. Critical for:
- Debugging agent failures
- Compliance audits
- Training and improvement

### Technical Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Applications                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐    │
│  │ Agent A  │  │ Agent B  │  │ Agent C  │  │  Human   │    │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘    │
│       │             │             │             │           │
├───────┴─────────────┴─────────────┴─────────────┴───────────┤
│                    Kyoyu Context Layer                       │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Context Graph Engine                                │    │
│  │  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐   │    │
│  │  │Decision │ │Evidence │ │ Memory  │ │ Handoff │   │    │
│  │  │ Traces  │ │ Store   │ │ Blocks  │ │Protocol │   │    │
│  │  └─────────┘ └─────────┘ └─────────┘ └─────────┘   │    │
│  └─────────────────────────────────────────────────────┘    │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Query & Retrieval                                   │    │
│  │  • Semantic search across context                    │    │
│  │  • Temporal queries (point-in-time)                  │    │
│  │  • Precedent matching                                │    │
│  │  • Context compression/summarization                 │    │
│  └─────────────────────────────────────────────────────┘    │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Governance & Access                                 │    │
│  │  • Role-based permissions                            │    │
│  │  • Audit logging                                     │    │
│  │  • Retention policies                                │    │
│  │  • Compliance exports                                │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
│                    Storage Layer                             │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐         │
│  │ Graph DB    │  │ Vector DB   │  │ Object Store│         │
│  │ (structure) │  │ (semantics) │  │ (artifacts) │         │
│  └─────────────┘  └─────────────┘  └─────────────┘         │
└─────────────────────────────────────────────────────────────┘
```

### Integration Points

- **Orchestration frameworks**: LangChain, LangGraph, CrewAI, AutoGen
- **Observability**: OpenTelemetry, Datadog, Arize, Langfuse
- **Vector stores**: Pinecone, Weaviate, Chroma
- **Identity**: Okta, Auth0, enterprise SSO

---

## 4. Competitive Landscape

### Direct Competitors

| Company | Focus | Limitation |
|---------|-------|------------|
| **Letta/MemGPT** | Agent memory | Single-agent focused, not multi-agent coordination |
| **LangGraph** | Workflow state | Scoped to workflow, not shared across systems |
| **Mem0** | Memory layer | Retrieval-focused, weak on decision traces |
| **Zep** | Long-term memory | Chat-oriented, not enterprise agent systems |

### Adjacent Players

| Company | Relationship |
|---------|--------------|
| **LangChain** ($1.25B) | Potential partner or acquirer - orchestration layer |
| **Arize/Langfuse** | Observability - complementary, could integrate |
| **MongoDB/Pinecone** | Storage layer - we build on top |
| **Datadog** | Could expand into agent context - future competitor |

### Differentiation

1. **Multi-agent native**: Built for coordination, not retrofitted
2. **Decision traces**: Not just retrieval - full reasoning lineage
3. **Handoff protocols**: First-class human-agent coordination
4. **Enterprise governance**: Compliance-ready from day one

---

## 5. Business Model

### Pricing Structure

**Usage-based pricing** aligned with value:

| Metric | Price | Rationale |
|--------|-------|-----------|
| Context traces/month | $0.001 per trace | Scales with agent activity |
| Storage (GB) | $0.10/GB/month | Context + artifacts |
| Query volume | $0.0001 per query | Retrieval usage |
| Active agents | $50/agent/month | Base platform fee |

### Example Customer Economics

**Mid-market customer** (20 agents, 1M traces/month):
- 20 agents × $50 = $1,000
- 1M traces × $0.001 = $1,000
- 50 GB storage = $5
- 500K queries = $50
- **Monthly: ~$2,055 → $24,660 ARR**

**Enterprise customer** (200 agents, 50M traces/month):
- 200 agents × $50 = $10,000
- 50M traces × $0.0008 (volume discount) = $40,000
- 500 GB storage = $50
- 10M queries = $800
- **Monthly: ~$50,850 → $610,200 ARR**

### Revenue Projections

| Year | Customers | ARR | Notes |
|------|-----------|-----|-------|
| Y1 | 15 | $500K | Design partners, free tier |
| Y2 | 80 | $3M | Product-market fit |
| Y3 | 300 | $15M | Scale go-to-market |
| Y4 | 800 | $45M | Enterprise expansion |
| Y5 | 1,500 | $100M | Market leader |

---

## 6. Go-to-Market Strategy

### Phase 1: Developer Adoption (Months 1-12)

**Target**: Individual developers and small teams building multi-agent systems

**Tactics**:
- Open-source SDK with generous free tier
- Deep integrations with LangChain, CrewAI, AutoGen
- Technical content (blog, tutorials, example repos)
- Developer community (Discord, GitHub)
- Conference presence (AI Engineer Summit, etc.)

**Goal**: 1,000 developers using free tier, 15 paying customers

### Phase 2: Mid-Market Expansion (Months 12-24)

**Target**: Companies with 5-50 agents in production

**Tactics**:
- Case studies from Phase 1 customers
- Self-serve upgrade path
- Solutions engineering team
- Industry-specific templates (fintech, healthcare, legal)

**Goal**: 80 paying customers, $3M ARR

### Phase 3: Enterprise (Months 24-36)

**Target**: Fortune 1000 with compliance requirements

**Tactics**:
- SOC 2, HIPAA, FedRAMP certifications
- Enterprise sales team
- Professional services
- Strategic partnerships (consulting firms, SIs)

**Goal**: 20 enterprise accounts at $200K+ ACV

### Channel Strategy

1. **Direct**: Developer self-serve → sales-assisted enterprise
2. **Partner**: LangChain/CrewAI ecosystem integrations
3. **Consulting**: Deloitte, Accenture AI practices
4. **Cloud marketplaces**: AWS, Azure, GCP

---

## 7. Team Requirements

### Founding Team (Pre-Seed)

| Role | Profile |
|------|---------|
| **CEO/Founder** | Technical background + enterprise sales experience |
| **CTO/Co-founder** | Distributed systems, databases, ML infrastructure |
| **Founding Engineer** | Full-stack, API design, developer experience |

### Seed Team (Post-funding)

- 2 additional backend engineers (distributed systems)
- 1 developer advocate
- 1 solutions engineer
- 1 designer (product + brand)

### Series A Team

- VP Engineering
- VP Sales
- 3-5 additional engineers
- 2 solutions engineers
- 2 sales reps

---

## 8. Risks & Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| **LangChain builds this** | Medium | High | Move fast, build community, create switching costs |
| **Orchestration frameworks fragment** | Medium | Medium | Support all major frameworks, stay neutral |
| **Enterprises build internally** | High | Medium | Make buy cheaper than build, emphasize governance |
| **Standards change** | Medium | Medium | Active participation in OpenTelemetry AI SIG |
| **Context windows get huge** | Low | High | Focus on governance/audit value, not just memory |

---

## 9. Funding Requirements

### Pre-Seed ($500K-1M)
- Source: Angels, pre-seed funds
- Use: 3-person team for 12 months
- Milestone: Working product, 5 design partners

### Seed ($3-5M)
- Source: Seed-stage VCs
- Use: Team of 8, GTM launch
- Milestone: $500K ARR, 50 customers

### Series A ($15-20M)
- Source: A-stage VCs
- Use: Scale team to 30, enterprise sales
- Milestone: $5M ARR, 5 enterprise customers

---

## 10. Why Kyoyu?

**Kyoyu (共有)** means "shared" or "common ownership" in Japanese. This isn't just a name - it's the product thesis:

- Agents **share** context, not just pass messages
- Decisions become **shared** knowledge across the organization
- The reasoning layer is **commonly owned** infrastructure

The name signals our differentiation: we're not building another orchestration framework. We're building the shared layer that makes orchestration actually work.

---

## Appendix: Key Sources

- [Foundation Capital: Context Graphs - AI's Trillion Dollar Opportunity](https://foundationcapital.com/context-graphs-ais-trillion-dollar-opportunity/)
- [Foundation Capital: Where AI is Headed in 2026](https://foundationcapital.com/where-ai-is-headed-in-2026/)
- [Gartner: Multiagent Systems in Enterprise AI](https://www.gartner.com/en/articles/multiagent-systems)
- [LangChain Series B Announcement](https://www.blog.langchain.com/series-b/)
- [OpenTelemetry: AI Agent Observability](https://opentelemetry.io/blog/2025/ai-agent-observability/)
- [Letta: Memory Blocks](https://www.letta.com/blog/memory-blocks)
- [Grand View Research: AI Agents Market](https://www.grandviewresearch.com/industry-analysis/ai-agents-market-report)
- [Deloitte: AI Agent Orchestration Predictions](https://www.deloitte.com/us/en/insights/industry/technology/technology-media-and-telecom-predictions/2026/ai-agent-orchestration.html)
