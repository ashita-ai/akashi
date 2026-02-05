# Akashi Strategic Direction

## Positioning

### The One-Liner
**"The black box recorder for AI decisions."**

When something goes wrong—or right—you'll know exactly why.

### The Analogy
Aviation black boxes don't fly planes. They prove what happened. Akashi doesn't orchestrate agents. It proves what they decided, why, and what they considered.

### Pricing Philosophy
**Price it like insurance, not like software.**

- Insurance is bought to manage risk, not to solve problems
- Buyers don't comparison-shop insurance on features
- The value is in not needing it until you do
- When you need it, it's invaluable

Akashi is lawsuit insurance. Audit insurance. "Explain what your AI did" insurance.

---

## Target Markets

### Primary: Startups Selling to Enterprise
**The Vanta Playbook**

Startups building AI agents don't care about compliance—until their enterprise prospect asks:
- "How do we audit what your AI decided?"
- "Can you prove your AI followed our policies?"
- "What happens when your AI makes a mistake?"

Without an answer, deals stall. Akashi is the answer that unstalls deals.

**Profile:**
- Series A-C startups
- Building AI agents that touch enterprise data
- Selling to Fortune 500 with security reviews
- Currently answering compliance questions with "we have logs"

**Message:** "Add AI governance in 5 minutes. Close enterprise deals."

### Secondary: Regulated Enterprises
**The Compliance Infrastructure Play**

Large organizations deploying AI agents in regulated contexts need:
- ISO 42001 compliance evidence
- EU AI Act Article 11 documentation
- Point-in-time audit capability ("what did the AI know on March 15th?")
- 10-year retention with immutable history

**Profile:**
- Financial services, healthcare, legal tech
- Internal AI agents handling sensitive decisions
- Compliance and GRC teams with audit requirements
- Budget holders who understand risk pricing

**Message:** "AI decision provenance for ISO 42001. Audit-ready from day one."

---

## Pricing Strategy

### Tier Structure

| Tier | Price | Target | Includes |
|------|-------|--------|----------|
| **Free** | $0 | Developers, OSS | 1 agent, 1K decisions/mo, 7-day retention |
| **Startup** | $149/mo | Pre-Series B | 10 agents, 50K decisions/mo, 90-day retention |
| **Scale** | $999/mo | Post-Series B | 100 agents, 500K decisions/mo, 1-year retention |
| **Enterprise** | Custom | Regulated orgs | Unlimited, 10-year retention, audit support, SLAs |

### Why This Works

1. **Free tier seeds the flywheel**: Developers try it, decisions accumulate, semantic search improves
2. **Startup tier is "deal insurance"**: $149/mo is nothing compared to a stalled $100K enterprise deal
3. **Scale tier grows with success**: Companies that close enterprise deals can afford $999/mo
4. **Enterprise tier captures value**: Regulated orgs pay for risk reduction, not features

### The Insurance Frame

Don't sell features. Sell outcomes:
- "What's it worth to close that enterprise deal?"
- "What's the cost of an AI audit you can't pass?"
- "What's the liability if you can't explain a decision?"

---

## Go-to-Market Motion

### Product-Led Growth (Primary)

```
Developer discovers Akashi
    → Adds MCP config (5 minutes)
    → Agent starts recording decisions
    → Founder sees dashboard showing decision history
    → Enterprise prospect asks about AI governance
    → Founder shows Akashi audit trail
    → Deal closes
    → Startup upgrades to paid tier
```

**Key enablers:**
- Frictionless onboarding (MCP = one config change)
- Self-serve signup and billing
- Clear value demonstration before paywall
- System prompt templates in major frameworks

### Sales-Led (Secondary)

For enterprise deals with:
- Security questionnaire requirements
- Procurement processes
- Custom retention/compliance needs
- Integration support requirements

**Key enablers:**
- ISO 42001 compliance mapping document
- SOC 2 Type II (eventually)
- Reference customers in regulated verticals
- Professional services for integration

---

## Moat Strategy

### Moat 1: Switching Costs (Data Gravity)

Once decisions are in Akashi:
- Historical audit trail lives there
- Semantic search quality depends on corpus size
- Compliance evidence can't be migrated easily
- Switching means losing the "black box" history

**How to deepen:** Make exports painful (compliance data should stay in compliance systems). Make the historical corpus increasingly valuable through better analytics.

### Moat 2: Ecosystem Integration

Be the decision store that every agent framework defaults to:
- LangChain middleware ships with Akashi integration
- CrewAI's default system prompt mentions Akashi
- AutoGen's documentation shows Akashi for audit trails

**How to deepen:** Contribute integrations upstream. Sponsor framework maintainers. Make Akashi the "obvious" choice for decision tracking.

### Moat 3: Regulatory Trust

Be the vendor that auditors recognize:
- Published ISO 42001 control mappings
- Case studies from regulated deployments
- Relationships with compliance consultancies
- Eventually: certification body partnerships

**How to deepen:** Get Big 4 firms to recommend Akashi. Get mentioned in compliance guidance documents. Become the de facto standard auditors expect.

### Moat 4: Network Effects (Weak but Real)

Cross-organization decision sharing:
- Agent at Company A queries decisions from Agent at Company B (with permission)
- Industry-wide precedent databases
- "What did other agents decide in similar situations?"

**How to deepen:** This is long-term. Requires critical mass and trust infrastructure. Don't optimize for this yet.

---

## Competitive Response

### If Langfuse Adds Decision Tracking

They have:
- Existing user base
- Brand recognition in observability
- Dashboard/UI expertise

They lack:
- Bi-temporal data model (hard to retrofit)
- Compliance-first positioning
- Agent-to-agent query mental model

**Response:** Emphasize the compliance angle they can't credibly claim. "Langfuse shows you what happened. Akashi proves it for auditors."

### If a Big Cloud Adds This

AWS/GCP/Azure could build decision stores.

**Response:** Multi-cloud. No lock-in. "Your AI governance layer shouldn't be owned by your cloud vendor."

### If Open Source Emerges

Someone forks the concept with an OSS version.

**Response:** Managed service value (we handle retention, compliance, uptime). Enterprise features (SSO, audit logs, SLAs). The Elastic/MongoDB playbook.

---

## Success Metrics

### Product-Market Fit Indicators

| Metric | Target | Why It Matters |
|--------|--------|----------------|
| Time to first decision recorded | < 10 min | Proves frictionless onboarding |
| Free → Paid conversion | > 5% | Proves value realization |
| Logo churn (monthly) | < 3% | Proves stickiness |
| NPS | > 50 | Proves satisfaction |
| "Would be very disappointed" | > 40% | Sean Ellis PMF test |

### Business Health Indicators

| Metric | Seed Target | Series A Target |
|--------|-------------|-----------------|
| ARR | $500K | $3M |
| Customers | 50 | 200 |
| Enterprise logos | 3 | 15 |
| Net revenue retention | 120% | 140% |

### Leading Indicators

- GitHub stars on SDK repos
- MCP config downloads
- "Akashi" mentions in AI agent discussions
- Inbound from compliance/GRC personas
- Enterprise security questionnaire requests

---

## 90-Day Priorities

### Month 1: PLG Foundation
- [ ] Self-serve signup flow
- [ ] Free tier implementation with limits
- [ ] Onboarding flow showing value (first decision → audit view)
- [ ] Billing integration (Stripe)

### Month 2: Ecosystem Seeding
- [ ] LangChain integration in their docs
- [ ] CrewAI integration PR
- [ ] "Getting Started" tutorial on dev.to / Medium
- [ ] System prompt templates promoted to framework maintainers

### Month 3: Compliance Credibility
- [ ] ISO 42001 control mapping document (PDF)
- [ ] First regulated vertical case study
- [ ] Compliance consultancy intro meetings
- [ ] Enterprise pricing page with "contact sales"

---

## Open Questions

1. **Dashboard or no dashboard?** Current design is API/MCP only. Should there be a minimal UI for showing audit trails to non-technical stakeholders?

2. **Multi-tenancy model?** How do startups show Akashi data to their enterprise customers during security reviews?

3. **Data residency?** EU customers may require EU-hosted instances. When does this become blocking?

4. **SOC 2 timing?** When do we need our own SOC 2 to sell to enterprises who require it?

5. **Pricing validation?** Are the proposed price points right? Need customer development interviews.
