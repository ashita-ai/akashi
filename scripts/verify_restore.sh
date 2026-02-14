#!/usr/bin/env bash
set -euo pipefail

# verify_restore.sh
#
# Post-restore verification helper for staging/ops drills.
# Requires:
#   - DATABASE_URL
# Optional:
#   - REBUILD_OUTBOX=true to repopulate Qdrant outbox from current decisions

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "error: DATABASE_URL is required" >&2
  exit 2
fi

echo "==> Running post-restore verification checks"

psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 <<'SQL'
\echo 'schema_migrations latest versions'
SELECT version, applied_at
FROM schema_migrations
ORDER BY applied_at DESC, version DESC
LIMIT 10;

\echo 'core table counts'
SELECT 'organizations' AS table_name, count(*)::bigint AS row_count FROM organizations
UNION ALL SELECT 'agents', count(*)::bigint FROM agents
UNION ALL SELECT 'agent_runs', count(*)::bigint FROM agent_runs
UNION ALL SELECT 'agent_events', count(*)::bigint FROM agent_events
UNION ALL SELECT 'decisions_current', count(*)::bigint FROM decisions WHERE valid_to IS NULL
UNION ALL SELECT 'alternatives', count(*)::bigint FROM alternatives
UNION ALL SELECT 'evidence', count(*)::bigint FROM evidence
UNION ALL SELECT 'integrity_proofs', count(*)::bigint FROM integrity_proofs
UNION ALL SELECT 'idempotency_keys', count(*)::bigint FROM idempotency_keys
UNION ALL SELECT 'mutation_audit_log', count(*)::bigint FROM mutation_audit_log
UNION ALL SELECT 'search_outbox', count(*)::bigint FROM search_outbox
UNION ALL SELECT 'search_outbox_dead_letters', count(*)::bigint FROM search_outbox_dead_letters
UNION ALL SELECT 'deletion_audit_log', count(*)::bigint FROM deletion_audit_log;

\echo 'orphan checks (must be 0)'
SELECT
  (SELECT count(*) FROM decisions d LEFT JOIN agent_runs r ON r.id = d.run_id WHERE r.id IS NULL) AS decisions_without_run,
  (SELECT count(*) FROM alternatives a LEFT JOIN decisions d ON d.id = a.decision_id WHERE d.id IS NULL) AS alternatives_without_decision,
  (SELECT count(*) FROM evidence e LEFT JOIN decisions d ON d.id = e.decision_id WHERE d.id IS NULL) AS evidence_without_decision;
SQL

if [[ "${REBUILD_OUTBOX:-false}" == "true" ]]; then
  echo "==> Rebuilding search_outbox from current decisions"
  psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO search_outbox (decision_id, org_id, operation)
SELECT id, org_id, 'upsert'
FROM decisions
WHERE valid_to IS NULL
  AND embedding IS NOT NULL
ON CONFLICT (decision_id, operation) DO UPDATE
  SET created_at = now(), attempts = 0, locked_until = NULL;
SQL
fi

echo "==> Restore verification complete"
