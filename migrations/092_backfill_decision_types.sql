-- 092: Backfill existing decisions with canonical decision types.
-- Uses the alias table seeded in migration 091 to normalize historical data.
-- Only updates current (non-superseded) decisions. Preserves the original
-- decision_type in metadata for audit trail forensics.

UPDATE decisions d
SET decision_type = a.canonical,
    metadata = d.metadata || jsonb_build_object('original_decision_type', d.decision_type)
FROM decision_type_aliases a
WHERE d.decision_type = a.alias
  AND d.org_id = a.org_id
  AND d.valid_to IS NULL;
