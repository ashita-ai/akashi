-- Migration 024: Composite Agent Identity (Spec 31)
--
-- Adds session_id and agent_context to decisions so every trace carries
-- multi-dimensional identity: who (agent_id), what session (session_id),
-- with what tool/model/task (agent_context JSONB).

ALTER TABLE decisions ADD COLUMN IF NOT EXISTS session_id UUID;
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS agent_context JSONB NOT NULL DEFAULT '{}';

-- Session-scoped queries: "show me all decisions from this session."
CREATE INDEX idx_decisions_session ON decisions (session_id, valid_from DESC)
  WHERE session_id IS NOT NULL;

-- Filtered queries on JSONB fields using btree on extracted text values.
CREATE INDEX idx_decisions_context_tool ON decisions
  USING btree ((agent_context->>'tool'))
  WHERE agent_context->>'tool' IS NOT NULL;

CREATE INDEX idx_decisions_context_model ON decisions
  USING btree ((agent_context->>'model'))
  WHERE agent_context->>'model' IS NOT NULL;

CREATE INDEX idx_decisions_context_repo ON decisions
  USING btree ((agent_context->>'repo'))
  WHERE agent_context->>'repo' IS NOT NULL;
