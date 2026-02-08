-- 015_fix_default_org_limits.sql
-- Fix default org's decision_limit and agent_limit to use 0 (unlimited)
-- instead of INT32_MAX (2147483647). The billing/metering layer treats 0 as
-- unlimited, but the original seed in 014 used INT32_MAX.

UPDATE organizations
SET decision_limit = 0,
    agent_limit    = 0,
    updated_at     = now()
WHERE id = '00000000-0000-0000-0000-000000000000'
  AND (decision_limit = 2147483647 OR agent_limit = 2147483647);
