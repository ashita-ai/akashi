# Akashi and ISO/IEC 42001 Alignment

[ISO/IEC 42001:2023](https://www.iso.org/standard/42001) is the first international standard for AI Management Systems (AIMS). It specifies requirements for establishing, implementing, and maintaining governance over AI systems.

Akashi's decision trace architecture provides technical infrastructure that supports several ISO 42001 requirements.

## Alignment Summary

| ISO 42001 Requirement | Akashi Capability |
|----------------------|------------------|
| **Risk identification and assessment** | Structured decision records with confidence scores indicate uncertainty. Conflict detection surfaces contradictory decisions between agents. |
| **Transparency and accountability** | Every decision includes reasoning, alternatives considered, and evidence. Bi-temporal queries answer "what did the system believe at time T?" |
| **Audit trails** | Append-only decision storage with transaction timestamps. Full history preserved via bi-temporal modeling (valid_from/valid_to). |
| **Performance review** | Quality scoring ranks decisions by completeness. Precedent reference tracking shows which decisions influence others. |
| **Stakeholder involvement** | Access grants enable cross-agent visibility. SSE subscriptions provide real-time decision feeds. |
| **Documentation** | Decision traces are machine-readable structured records, queryable via SQL, semantic search, or MCP tools. |

## Technical Mapping

### 4.1 Understanding the organization and its context

Akashi captures organizational AI decision context through:
- Agent registration with roles (admin/agent/reader)
- Run metadata associating decisions with execution context
- Evidence records with source provenance

### 6.1 Actions to address risks and opportunities

Decision traces include:
- **Confidence scores** (0.0-1.0) — quantified uncertainty
- **Alternatives considered** — what else was evaluated
- **Rejection reasons** — why alternatives were not selected
- **Conflict detection** — automated identification of contradictory decisions

### 7.5 Documented information

Akashi maintains:
- Immutable decision records with unique IDs
- Bi-temporal versioning (business time + system time)
- Transaction timestamps for audit trail
- JSONB metadata for extensibility

### 9.1 Monitoring, measurement, analysis and evaluation

Query capabilities support:
- Structured queries by agent, decision type, confidence threshold, time range
- Semantic search for similar past decisions
- Temporal queries for point-in-time analysis
- Quality scoring for decision completeness

### 10.2 Nonconformity and corrective action

When decisions conflict:
- Materialized view detects same-type decisions with different outcomes
- Conflicts surfaced via `/v1/conflicts` endpoint
- MCP `akashi_check` tool returns conflicts for a decision type

## Data Model for Audit

Each decision record contains:

```json
{
  "id": "uuid",
  "run_id": "uuid",
  "agent_id": "string",
  "decision_type": "model_selection | architecture | ...",
  "outcome": "what was decided",
  "confidence": 0.85,
  "reasoning": "why this choice",
  "alternatives": [
    {"label": "option A", "selected": true, "score": 0.9},
    {"label": "option B", "selected": false, "score": 0.7, "rejection_reason": "too slow"}
  ],
  "evidence": [
    {"source_type": "benchmark", "content": "...", "source_uri": "..."}
  ],
  "valid_from": "2026-02-04T12:00:00Z",
  "valid_to": null,
  "transaction_time": "2026-02-04T12:00:01Z",
  "quality_score": 0.75,
  "precedent_ref": "uuid of prior decision that influenced this one"
}
```

## Certification Considerations

ISO 42001 certification requires:
1. Documented AIMS policies and procedures
2. Evidence of implementation
3. Internal audit records
4. Management review records

Akashi provides the **evidence of implementation** layer — structured, queryable records of AI system decisions. It does not replace policy documentation or management processes, but provides the technical audit trail those processes require.

## Integration Pattern

For organizations pursuing ISO 42001 certification:

1. **Define decision taxonomy** — Map AI decision types to Akashi's decision_type field
2. **Instrument agents** — Use SDK middleware to enforce check-before/record-after pattern
3. **Configure access control** — Set up agent roles matching organizational structure
4. **Export for audit** — Query decisions by time range for certification evidence
5. **Monitor conflicts** — Subscribe to conflict notifications for governance alerts

## References

- [ISO/IEC 42001:2023](https://www.iso.org/standard/42001)
- [BSI ISO 42001 Guide](https://www.bsigroup.com/en-US/products-and-services/standards/iso-42001-ai-management-system/)
- [Microsoft ISO 42001 Compliance](https://learn.microsoft.com/en-us/compliance/regulatory/offering-iso-42001)
