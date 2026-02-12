-- 022: Add PostgreSQL full-text search to the decisions table.
--
-- Replaces the AND-all-terms ILIKE approach with proper tsvector/tsquery
-- search that handles stop word removal, stemming, and relevance ranking.
-- The tsvector column is maintained by a trigger on insert/update.

-- Add tsvector column for combined full-text search.
ALTER TABLE decisions ADD COLUMN search_vector tsvector;

-- Backfill existing rows with weighted tsvector:
-- A = outcome (most important), B = decision_type, C = reasoning.
UPDATE decisions SET search_vector =
  setweight(to_tsvector('english', COALESCE(outcome, '')), 'A') ||
  setweight(to_tsvector('english', COALESCE(decision_type, '')), 'B') ||
  setweight(to_tsvector('english', COALESCE(reasoning, '')), 'C');

-- GIN index for fast @@ queries.
CREATE INDEX idx_decisions_search_vector ON decisions USING gin(search_vector);

-- Trigger function to maintain search_vector on insert/update.
CREATE OR REPLACE FUNCTION decisions_search_vector_update() RETURNS trigger AS $$
BEGIN
  NEW.search_vector :=
    setweight(to_tsvector('english', COALESCE(NEW.outcome, '')), 'A') ||
    setweight(to_tsvector('english', COALESCE(NEW.decision_type, '')), 'B') ||
    setweight(to_tsvector('english', COALESCE(NEW.reasoning, '')), 'C');
  RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER decisions_search_vector_trigger
  BEFORE INSERT OR UPDATE OF outcome, decision_type, reasoning ON decisions
  FOR EACH ROW EXECUTE FUNCTION decisions_search_vector_update();
