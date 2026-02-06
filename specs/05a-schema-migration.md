# Spec 05a: Schema + Migration 014

**Status**: Ready for implementation
**Phase**: 1 of 5 (Multi-Tenancy)
**Depends on**: Nothing
**Blocks**: Phase 2 (05b), Phase 3 (05c), Phase 4 (05d), Phase 5 (05e)

## Goal

Create migration 014 that adds multi-tenancy schema support: `organizations` table, `org_id` columns on all tenant-scoped tables, usage tracking, email verification, and RLS policies. Existing data is assigned to a default organization.

## Deliverables

1. `migrations/014_multi_tenancy.sql` — the migration file
2. Updated materialized views that include `org_id` in join conditions
3. All existing tests continue to pass (migration runs against test containers)

## Migration SQL

Create `migrations/014_multi_tenancy.sql` with the following content. This is the **exact** SQL to write — do not deviate.

```sql
-- 014_multi_tenancy.sql
-- Adds multi-tenancy: organizations, org_id on tenant tables, usage tracking,
-- email verification, RLS policies.

-- =============================================================================
-- 1. Organizations table
-- =============================================================================

CREATE TABLE organizations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,
    plan            TEXT NOT NULL DEFAULT 'free'
                    CHECK (plan IN ('free', 'pro', 'enterprise')),
    stripe_customer_id     TEXT UNIQUE,
    stripe_subscription_id TEXT UNIQUE,
    decision_limit  INTEGER NOT NULL DEFAULT 1000,
    agent_limit     INTEGER NOT NULL DEFAULT 1,
    email           TEXT NOT NULL,
    email_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_organizations_slug
    ON organizations (slug);
CREATE INDEX idx_organizations_stripe_customer
    ON organizations (stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

-- =============================================================================
-- 2. Default organization for existing data
-- =============================================================================

INSERT INTO organizations (id, name, slug, plan, email, email_verified, decision_limit, agent_limit)
VALUES (
    '00000000-0000-0000-0000-000000000000',
    'Default',
    'default',
    'enterprise',
    'admin@localhost',
    true,
    2147483647,
    2147483647
);

-- =============================================================================
-- 3. Add org_id to tenant-scoped tables
-- =============================================================================

-- agents: add org_id, backfill, make NOT NULL, re-constrain uniqueness
ALTER TABLE agents
    ADD COLUMN org_id UUID REFERENCES organizations(id);
UPDATE agents SET org_id = '00000000-0000-0000-0000-000000000000';
ALTER TABLE agents ALTER COLUMN org_id SET NOT NULL;

-- Drop old unique constraint on agent_id (was globally unique).
-- The constraint name may vary; use the index name from the schema.
DO $$
BEGIN
    -- Drop any unique index on agents(agent_id) alone.
    IF EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'agents'
          AND indexdef LIKE '%UNIQUE%'
          AND indexdef LIKE '%agent_id%'
          AND indexdef NOT LIKE '%org_id%'
    ) THEN
        -- Find and drop the constraint by name.
        EXECUTE (
            SELECT format('ALTER TABLE agents DROP CONSTRAINT %I', conname)
            FROM pg_constraint
            WHERE conrelid = 'agents'::regclass
              AND contype = 'u'
              AND array_length(conkey, 1) = 1
            LIMIT 1
        );
    END IF;
END $$;

-- New unique constraint: agent_id unique within org, not globally.
CREATE UNIQUE INDEX idx_agents_org_agent ON agents (org_id, agent_id);

-- agent_runs: add org_id, backfill, make NOT NULL
ALTER TABLE agent_runs
    ADD COLUMN org_id UUID REFERENCES organizations(id);
UPDATE agent_runs SET org_id = '00000000-0000-0000-0000-000000000000';
ALTER TABLE agent_runs ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX idx_agent_runs_org ON agent_runs (org_id, id);

-- agent_events: add org_id, backfill, make NOT NULL
-- Note: TimescaleDB hypertables don't support foreign keys, so no REFERENCES.
ALTER TABLE agent_events
    ADD COLUMN org_id UUID;
UPDATE agent_events SET org_id = '00000000-0000-0000-0000-000000000000';
ALTER TABLE agent_events ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX idx_agent_events_org ON agent_events (org_id);

-- decisions: add org_id, backfill, make NOT NULL
ALTER TABLE decisions
    ADD COLUMN org_id UUID REFERENCES organizations(id);
UPDATE decisions SET org_id = '00000000-0000-0000-0000-000000000000';
ALTER TABLE decisions ALTER COLUMN org_id SET NOT NULL;

-- Partial index for current decisions scoped by org.
CREATE INDEX idx_decisions_org_agent_current
    ON decisions (org_id, agent_id, valid_from DESC)
    WHERE valid_to IS NULL;

-- access_grants: add org_id, backfill, make NOT NULL
ALTER TABLE access_grants
    ADD COLUMN org_id UUID REFERENCES organizations(id);
UPDATE access_grants SET org_id = '00000000-0000-0000-0000-000000000000';
ALTER TABLE access_grants ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX idx_access_grants_org ON access_grants (org_id, grantee_id);

-- =============================================================================
-- 4. Usage tracking table
-- =============================================================================

CREATE TABLE org_usage (
    org_id          UUID NOT NULL REFERENCES organizations(id),
    period          TEXT NOT NULL,  -- 'YYYY-MM' format
    decision_count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, period)
);

-- =============================================================================
-- 5. Email verification table
-- =============================================================================

CREATE TABLE email_verifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    token       TEXT NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =============================================================================
-- 6. Update role CHECK constraint on agents
-- =============================================================================

-- Drop old CHECK constraint on role, add new one with org_owner and platform_admin.
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid = 'agents'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%role%';

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE agents DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE agents
    ADD CONSTRAINT agents_role_check
    CHECK (role IN ('platform_admin', 'org_owner', 'admin', 'agent', 'reader'));

-- =============================================================================
-- 7. Recreate materialized views with org_id awareness
-- =============================================================================

-- decision_conflicts: scope conflicts within the same org
DROP MATERIALIZED VIEW IF EXISTS decision_conflicts;

CREATE MATERIALIZED VIEW decision_conflicts AS
SELECT
    d1.id AS decision_a_id,
    d2.id AS decision_b_id,
    d1.org_id,
    d1.agent_id AS agent_a,
    d2.agent_id AS agent_b,
    d1.run_id AS run_a,
    d2.run_id AS run_b,
    d1.decision_type,
    d1.outcome AS outcome_a,
    d2.outcome AS outcome_b,
    d1.confidence AS confidence_a,
    d2.confidence AS confidence_b,
    d1.valid_from AS decided_at_a,
    d2.valid_from AS decided_at_b,
    GREATEST(d1.valid_from, d2.valid_from) AS detected_at
FROM decisions d1
JOIN decisions d2
    ON d1.decision_type = d2.decision_type
    AND d1.org_id = d2.org_id          -- same org only
    AND d1.agent_id != d2.agent_id
    AND d1.outcome != d2.outcome
    AND d1.valid_to IS NULL
    AND d2.valid_to IS NULL
    AND d1.id < d2.id
    AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 3600
WITH DATA;

CREATE UNIQUE INDEX idx_decision_conflicts_pair
    ON decision_conflicts(decision_a_id, decision_b_id);

-- agent_current_state: add org_id
DROP MATERIALIZED VIEW IF EXISTS agent_current_state;

CREATE MATERIALIZED VIEW agent_current_state AS
SELECT
    ar.agent_id,
    ar.org_id,
    ar.id AS latest_run_id,
    ar.status AS run_status,
    ar.started_at,
    COUNT(ae.id) AS event_count,
    MAX(ae.occurred_at) AS last_activity,
    (SELECT COUNT(*) FROM decisions d
     WHERE d.agent_id = ar.agent_id
       AND d.org_id = ar.org_id
       AND d.valid_to IS NULL) AS active_decisions
FROM agent_runs ar
LEFT JOIN agent_events ae ON ae.run_id = ar.id
WHERE ar.started_at = (
    SELECT MAX(started_at)
    FROM agent_runs
    WHERE agent_id = ar.agent_id AND org_id = ar.org_id
)
GROUP BY ar.agent_id, ar.org_id, ar.id, ar.status, ar.started_at
WITH DATA;

-- current_decisions view: add org_id
DROP VIEW IF EXISTS current_decisions;

CREATE VIEW current_decisions AS
SELECT * FROM decisions
WHERE valid_to IS NULL
ORDER BY valid_from DESC;

-- =============================================================================
-- 8. Row-Level Security (defense in depth)
-- =============================================================================
-- RLS policies enforce org isolation at the database level. The application
-- sets `SET LOCAL app.org_id = '<uuid>'` per transaction. Even if application
-- code has a bug in its WHERE clause, RLS prevents cross-org data leakage.
--
-- IMPORTANT: RLS does NOT restrict the table owner role. The application must
-- either use SET ROLE to a restricted role, or connect as a non-owner role
-- for RLS to be effective. This is a deployment concern — documented here
-- and in the operational guide.

ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_grants ENABLE ROW LEVEL SECURITY;

-- Create a restricted role for the application to SET ROLE to.
-- Skip if it already exists (idempotent).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'akashi_app') THEN
        CREATE ROLE akashi_app;
    END IF;
END $$;

-- Grant necessary permissions to akashi_app.
GRANT SELECT, INSERT, UPDATE, DELETE ON agents TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_runs TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_events TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON decisions TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON alternatives TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON evidence TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON access_grants TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON organizations TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON org_usage TO akashi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON email_verifications TO akashi_app;
GRANT USAGE ON SEQUENCE event_sequence_num_seq TO akashi_app;

-- RLS policies: read and write scoped to current org.
CREATE POLICY org_isolation_agents ON agents
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

CREATE POLICY org_isolation_runs ON agent_runs
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

CREATE POLICY org_isolation_decisions ON decisions
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

CREATE POLICY org_isolation_grants ON access_grants
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);
```

## Verification

After migration runs:

1. `SELECT count(*) FROM organizations` returns 1 (the default org)
2. `SELECT org_id FROM agents LIMIT 1` returns `00000000-0000-0000-0000-000000000000`
3. `SELECT org_id FROM decisions LIMIT 1` returns `00000000-0000-0000-0000-000000000000`
4. `\d agents` shows `org_id UUID NOT NULL` column
5. `\di idx_agents_org_agent` shows the new unique index
6. RLS is enabled: `SELECT relname, relrowsecurity FROM pg_class WHERE relname IN ('agents', 'agent_runs', 'decisions', 'access_grants')` all show `true`
7. All existing tests pass: `go test -race ./...`

## Files Changed

| File | Action |
|------|--------|
| `migrations/014_multi_tenancy.sql` | **Create** |

## Notes

- `alternatives` and `evidence` do NOT get `org_id` — they're always accessed through their parent `decision` which has `org_id`. This avoids redundant columns and keeps COPY operations simpler.
- `agent_events` gets `org_id` but no FK because TimescaleDB hypertables don't support foreign keys. The constraint is enforced in application code.
- The default org UUID `00000000-0000-0000-0000-000000000000` is a sentinel value for pre-migration data. All new orgs use `gen_random_uuid()`.
- RLS uses `current_setting('app.org_id', true)` — the `true` parameter makes it return NULL instead of erroring when the setting isn't set, which is safer for superuser/migration contexts.
