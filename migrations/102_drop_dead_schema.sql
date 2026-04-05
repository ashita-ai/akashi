-- 102: Drop dead schema objects (evidence_orphans table, current_decisions view).
-- Neither is referenced in any Go production code. evidence_orphans was a one-time
-- archive from migration 025. current_decisions was a convenience view from migration
-- 001 that was never queried by the application.

DROP TABLE IF EXISTS evidence_orphans;
DROP VIEW IF EXISTS current_decisions;
