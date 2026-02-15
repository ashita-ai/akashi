-- 038: Add last_seen timestamp to agents for activity tracking.
-- Populated on every authenticated API request (JWT or API key).
-- NULL means the agent has never made an authenticated request.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS last_seen TIMESTAMPTZ;
