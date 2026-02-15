#!/usr/bin/env bash
set -euo pipefail

# archive_agent_events.sh
#
# Archive-before-purge lifecycle helper for agent_events.
# Defaults are safety-first:
#   - DRY_RUN=true
#   - ENABLE_PURGE=false
#
# Required:
#   DATABASE_URL
#
# Optional:
#   RETAIN_DAYS        default: 90
#   BATCH_DAYS         default: 1
#   DRY_RUN            default: true
#   ENABLE_PURGE       default: false (requires DRY_RUN=false)

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "error: DATABASE_URL is required" >&2
  exit 2
fi

RETAIN_DAYS="${RETAIN_DAYS:-90}"
BATCH_DAYS="${BATCH_DAYS:-1}"
DRY_RUN="${DRY_RUN:-true}"
ENABLE_PURGE="${ENABLE_PURGE:-false}"

# Validate numeric inputs to prevent SQL injection (issue #59).
[[ "${RETAIN_DAYS}" =~ ^[0-9]+$ ]] || { echo "error: RETAIN_DAYS must be a positive integer, got '${RETAIN_DAYS}'" >&2; exit 2; }
[[ "${BATCH_DAYS}" =~ ^[0-9]+$ ]] || { echo "error: BATCH_DAYS must be a positive integer, got '${BATCH_DAYS}'" >&2; exit 2; }

if [[ "${ENABLE_PURGE}" == "true" && "${DRY_RUN}" != "false" ]]; then
  echo "error: ENABLE_PURGE=true requires DRY_RUN=false" >&2
  exit 2
fi

echo "==> agent_events archival configuration"
echo "    RETAIN_DAYS=${RETAIN_DAYS}"
echo "    BATCH_DAYS=${BATCH_DAYS}"
echo "    DRY_RUN=${DRY_RUN}"
echo "    ENABLE_PURGE=${ENABLE_PURGE}"

# Identify one bounded time window per run to reduce lock pressure.
# The script can be run repeatedly (e.g., cron) until old ranges are exhausted.
WINDOW_SQL=$(cat <<SQL
WITH bounds AS (
  SELECT
    min(occurred_at) AS oldest,
    now() - ('${RETAIN_DAYS} days')::interval AS cutoff
  FROM agent_events
),
windowed AS (
  SELECT
    oldest AS start_at,
    LEAST(oldest + ('${BATCH_DAYS} days')::interval, cutoff) AS end_at
  FROM bounds
  WHERE oldest IS NOT NULL
    AND oldest < cutoff
)
SELECT
  to_char(start_at, 'YYYY-MM-DD"T"HH24:MI:SSOF'),
  to_char(end_at, 'YYYY-MM-DD"T"HH24:MI:SSOF')
FROM windowed;
SQL
)

WINDOW_RESULT=$(psql "${DATABASE_URL}" -At -v ON_ERROR_STOP=1 -c "${WINDOW_SQL}" || true)
if [[ -z "${WINDOW_RESULT}" ]]; then
  echo "==> Nothing to archive: no rows older than retention cutoff"
  exit 0
fi

START_AT="${WINDOW_RESULT%%|*}"
END_AT="${WINDOW_RESULT##*|}"

echo "==> Processing window: [${START_AT}, ${END_AT})"

ARCHIVE_SQL=$(cat <<SQL
WITH candidate AS (
  SELECT id, run_id, event_type, sequence_num, occurred_at, agent_id, payload, org_id, created_at
  FROM agent_events
  WHERE occurred_at >= '${START_AT}'::timestamptz
    AND occurred_at <  '${END_AT}'::timestamptz
),
ins AS (
  INSERT INTO agent_events_archive (
    id, run_id, event_type, sequence_num, occurred_at,
    agent_id, payload, org_id, created_at
  )
  SELECT
    c.id, c.run_id, c.event_type, c.sequence_num, c.occurred_at,
    c.agent_id, c.payload, c.org_id, c.created_at
  FROM candidate c
  ON CONFLICT (id, occurred_at) DO NOTHING
  RETURNING 1
)
SELECT
  (SELECT count(*) FROM candidate) AS candidate_count,
  (SELECT count(*) FROM ins) AS inserted_count;
SQL
)

if [[ "${DRY_RUN}" == "true" ]]; then
  echo "==> Archiving rows into agent_events_archive (dry-run mode)"
  psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -c "${ARCHIVE_SQL}"
  echo "==> DRY_RUN=true, skipping purge step"
  exit 0
fi

if [[ "${ENABLE_PURGE}" != "true" ]]; then
  echo "==> Archiving rows into agent_events_archive"
  psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -c "${ARCHIVE_SQL}"
  echo "==> ENABLE_PURGE=false, archive-only mode complete"
  exit 0
fi

ARCHIVE_AND_PURGE_SQL=$(cat <<SQL
BEGIN;
WITH candidate AS (
  SELECT id, run_id, event_type, sequence_num, occurred_at, agent_id, payload, org_id, created_at
  FROM agent_events
  WHERE occurred_at >= '${START_AT}'::timestamptz
    AND occurred_at <  '${END_AT}'::timestamptz
),
ins AS (
  INSERT INTO agent_events_archive (
    id, run_id, event_type, sequence_num, occurred_at,
    agent_id, payload, org_id, created_at
  )
  SELECT
    c.id, c.run_id, c.event_type, c.sequence_num, c.occurred_at,
    c.agent_id, c.payload, c.org_id, c.created_at
  FROM candidate c
  ON CONFLICT (id, occurred_at) DO NOTHING
  RETURNING id, occurred_at
),
deleted AS (
  DELETE FROM agent_events e
  USING agent_events_archive a
  WHERE e.id = a.id
    AND e.occurred_at = a.occurred_at
    AND e.occurred_at >= '${START_AT}'::timestamptz
    AND e.occurred_at <  '${END_AT}'::timestamptz
  RETURNING 1
)
SELECT
  (SELECT count(*) FROM candidate) AS candidate_count,
  (SELECT count(*) FROM ins) AS inserted_count,
  (SELECT count(*) FROM deleted) AS purged_count;
COMMIT;
SQL
)

echo "==> Archiving and purging in one transaction (archive-first verified by join)"
psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -c "${ARCHIVE_AND_PURGE_SQL}"

echo "==> archive_agent_events completed"
