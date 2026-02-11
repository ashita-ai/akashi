-- 024_drop_billing_columns.sql
-- Removes enterprise billing columns from the organizations table.
-- The organizations table itself stays (FK target for org_id columns).

ALTER TABLE organizations DROP COLUMN IF EXISTS stripe_customer_id;
ALTER TABLE organizations DROP COLUMN IF EXISTS stripe_subscription_id;
ALTER TABLE organizations DROP COLUMN IF EXISTS decision_limit;
ALTER TABLE organizations DROP COLUMN IF EXISTS agent_limit;
ALTER TABLE organizations DROP COLUMN IF EXISTS email;
ALTER TABLE organizations DROP COLUMN IF EXISTS email_verified;
ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_plan_check;
ALTER TABLE organizations ALTER COLUMN plan SET DEFAULT 'enterprise';

-- Drop usage tracking table.
DROP TABLE IF EXISTS org_usage;

-- Drop email verifications table.
DROP TABLE IF EXISTS email_verifications;

-- Drop billing-related indexes.
DROP INDEX IF EXISTS idx_organizations_stripe_customer;
