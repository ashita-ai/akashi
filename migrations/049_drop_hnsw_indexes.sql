-- 049: Drop pgvector HNSW indexes now that all ANN goes through Qdrant.
--
-- Background: Akashi uses Qdrant as the authoritative ANN layer for both
-- user-facing semantic search (akashi_check, /v1/search) and internal uses
-- (conflict candidate discovery, consensus scoring). Maintaining HNSW indexes
-- in Postgres alongside Qdrant is architecturally incoherent and expensive:
--
--   idx_decisions_embedding:         ~4.25 GB RAM per 1M decisions (1024-dim float32, m=16)
--   idx_decisions_outcome_embedding: ~4.25 GB RAM per 1M decisions (dead weight — pgvector
--                                    cannot use two HNSW indexes in one query)
--   idx_evidence_embedding:          ~12.7 GB RAM per 1M evidence rows
--
-- After this migration, Postgres stores vector columns as source of truth only.
-- Qdrant owns all approximate nearest-neighbor queries. When Qdrant is
-- unavailable, semantic search degrades to text search (surfaced to callers
-- via the X-Search-Backend: text response header); conflict detection skips
-- candidate retrieval until Qdrant is reachable again.
--
-- The vector columns (embedding, outcome_embedding) are retained — they are the
-- source of truth used to populate Qdrant and for pairwise cosine similarity
-- comparisons in Go after Qdrant returns candidate IDs.

DROP INDEX IF EXISTS idx_decisions_embedding;
DROP INDEX IF EXISTS idx_decisions_outcome_embedding;
DROP INDEX IF EXISTS idx_evidence_embedding;
