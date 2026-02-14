-- 022: Add PostgreSQL full-text search to the decisions table.
--
-- Replaces the AND-all-terms ILIKE approach with proper tsvector/tsquery
-- search that handles stop word removal, stemming, and relevance ranking.
-- The tsvector column is maintained by a trigger on insert/update.
--
-- Search scope: outcome, decision_type, reasoning only. Metadata, alternatives,
-- and evidence are excluded by design; application filters those separately.
--
-- Language: ts_config 'english' is intentional. Multi-language support would
-- require per-tenant or per-row config.
--
-- Note: If the trigger is dropped or disabled, search_vector will be NULL
-- for new/updated rows and those rows won't match FTS queries.
--
-- WARNING: On very large tables, the batched backfill may hold a transaction
-- for several minutes. Consider running during low-traffic windows.
--
-- Note: We use CREATE INDEX (not CONCURRENTLY) so this runs inside the app's
-- migration transaction. For production with millions of rows, run
-- CREATE INDEX CONCURRENTLY manually during a maintenance window if needed.

-- Add tsvector column for combined full-text search.
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS search_vector tsvector;

-- Trigger function to maintain search_vector on insert/update.
-- Created BEFORE backfill so concurrent inserts during migration get search_vector.
CREATE OR REPLACE FUNCTION decisions_search_vector_update() RETURNS trigger AS $$
BEGIN
  NEW.search_vector :=
    setweight(to_tsvector('english', COALESCE(NEW.outcome, '')), 'A') ||
    setweight(to_tsvector('english', COALESCE(NEW.decision_type, '')), 'B') ||
    setweight(to_tsvector('english', COALESCE(NEW.reasoning, '')), 'C');
  RETURN NEW;
END
$$ LANGUAGE plpgsql;

-- Drop first so migration is idempotent; recreate to pick up any function changes.
DROP TRIGGER IF EXISTS decisions_search_vector_trigger ON decisions;
CREATE TRIGGER decisions_search_vector_trigger
  BEFORE INSERT OR UPDATE OF outcome, decision_type, reasoning ON decisions
  FOR EACH ROW EXECUTE FUNCTION decisions_search_vector_update();

-- Backfill existing rows with weighted tsvector. Run AFTER trigger creation so
-- rows inserted during migration get search_vector from the trigger; this
-- catches pre-existing rows and any inserted before the trigger existed.
-- Batched (10k per iteration) to limit lock duration per pass on large tables.
-- A = outcome (most important), B = decision_type, C = reasoning.
DO $$
DECLARE
  batch_count int;
BEGIN
  LOOP
    WITH batch AS (
      SELECT id, outcome, decision_type, reasoning
      FROM decisions
      WHERE search_vector IS NULL
      LIMIT 10000
    )
    UPDATE decisions d
    SET search_vector =
      setweight(to_tsvector('english', COALESCE(b.outcome, '')), 'A') ||
      setweight(to_tsvector('english', COALESCE(b.decision_type, '')), 'B') ||
      setweight(to_tsvector('english', COALESCE(b.reasoning, '')), 'C')
    FROM batch b
    WHERE d.id = b.id;
    GET DIAGNOSTICS batch_count = ROW_COUNT;
    EXIT WHEN batch_count = 0;
  END LOOP;
END $$;

-- GIN index for fast @@ queries.
CREATE INDEX IF NOT EXISTS idx_decisions_search_vector ON decisions USING gin(search_vector);
