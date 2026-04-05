-- 097: Fix remaining decisions with NULL or incorrect project assignments.
--
-- Migration 095 fixed workspace names that leaked via the filepath.Base bug,
-- but only caught cases where project_submitted existed as a correction hint.
-- This migration handles two remaining categories:
--
--   1. Decisions with NULL project (traced before project was required, or
--      traced without agent_context). Identified by content and assigned to
--      the correct project (akashi or tessera).
--
--   2. Decisions still carrying Conductor workspace names (boston, salvador,
--      palembang, etc.) that 095's heuristic couldn't resolve because they
--      lacked a project_submitted breadcrumb. Mapped by decision ID to the
--      correct project based on what the decision actually changed.
--
-- The GENERATED project column auto-recomputes from agent_context.

-- ============================================================
-- NULL → akashi (decisions about akashi server, SDKs, tooling)
-- ============================================================
UPDATE decisions
SET agent_context = jsonb_set(
    COALESCE(agent_context, '{}'::jsonb),
    '{client,project}',
    '"akashi"'::jsonb,
    true
)
WHERE id IN (
    '91631c33-2b51-4592-8691-a6f87e4b6489',  -- project field required on trace entry points
    'bbf9a4f8-d917-4ef2-a5d5-b4cd04d4889e',  -- 6-axis audit of akashi codebase
    'c1c5e13f-64a9-48d0-8643-8b693a0a9068',  -- 15 missing soft-delete filters
    '85253ca8-dcbd-457a-b710-80bfc1897ba1',   -- supersession_velocity JSON tag rename
    '34928025-e268-47d5-a824-7e031643b726',   -- SDK types aligned to server model
    '197e3a6d-3101-41ac-b025-799add51dcb7',   -- org_id added to 5 storage queries
    'ca9f7d42-02a7-4654-8197-854ac424b49d',   -- ErrCodeServiceUnavailable added
    'a6d5f451-d2b7-4fce-9645-4737a96de062',   -- golangci-lint version alignment
    'ddcef804-a9ef-47c4-b8c3-b2b29c449d63'    -- /v1 prefix on HTTP routes
)
AND valid_to IS NULL;

-- ============================================================
-- NULL → tessera
-- ============================================================
UPDATE decisions
SET agent_context = jsonb_set(
    COALESCE(agent_context, '{}'::jsonb),
    '{client,project}',
    '"tessera"'::jsonb,
    true
)
WHERE id IN (
    'd4d2f8db-61b2-4d29-9cb0-0ed330ee81fb',  -- Dockerfile node:20-slim frontend builder
    '1b441303-177e-4efc-9814-cda112040a0e'    -- tessera emits to akashi integration design
)
AND valid_to IS NULL;

-- ============================================================
-- Workspace names → akashi
-- ============================================================
UPDATE decisions
SET agent_context = jsonb_set(
    COALESCE(agent_context, '{}'::jsonb),
    '{client,project}',
    '"akashi"'::jsonb,
    true
)
WHERE id IN (
    -- salvador: parallelized scoreForDecision
    '73b2c1d0-6cc0-4b4c-8f98-71e7d27486c6',
    -- boston: PRs #608 and #613 (akashi event buffer + flush atomicity)
    '94facf52-ec2b-411d-bfe9-69ef9866e8f6',
    'a8a2a011-ba29-4d6f-a30c-33467ad69ac6',
    'dcf00edb-fc92-4b86-9c98-8e910ac70ae5',
    '40280a05-aa2a-48ca-9f2f-6fb42677b17c',
    -- hanoi-v1: CLAUDE.md PR requirement format change
    '6767dd14-6163-44bc-99d8-99e508244728'
)
AND valid_to IS NULL;

-- ============================================================
-- Workspace names → tessera
-- ============================================================
UPDATE decisions
SET agent_context = jsonb_set(
    COALESCE(agent_context, '{}'::jsonb),
    '{client,project}',
    '"tessera"'::jsonb,
    true
)
WHERE id IN (
    -- palembang: repo sync worker (Python)
    '133c91ea-a834-4e5f-9963-b32f6d63354e',
    '2a2b6947-2c62-4907-adb5-819517fb2e7c',
    'e515d092-f0b5-4764-9123-d008b69f9ba9',
    '5979e765-d2d4-4a0c-9c04-209b2ba9336b',
    'd8e560e2-4da6-47b2-974b-485c9b6ba52c',
    -- valencia-v1: FQN injection fix in gRPC/GraphQL sync
    '28dff32a-e572-4d44-b8f0-ea3c9a3433fa',
    -- denpasar: force-publish endpoint
    'f0221f95-0cb2-41eb-8547-d0e2b49c86d3',
    -- kathmandu-v2: graph API PR #430
    '680313cb-e84c-4591-bce2-d9de558477c8',
    -- sao-paulo-v1: service dependency graph, error extraction, merge conflicts
    '4653a5b1-c918-471c-8e72-18ca8c0dc3f4',
    '1e5ee838-a6c3-44f3-b25f-ab96c81edf9f',
    '3ec31abd-95a0-4fd9-bcbc-afc4059debf0',
    'dfffffa3-a367-4ed8-b890-15c0b558ec95',
    'cacc3ba3-faf4-4987-85a3-db6793641ff0',
    -- winnipeg: OTEL dependency discovery
    'f15a9f3b-fc7e-4e7b-9db6-7faa8464d71b',
    'f01b0d08-88ee-447f-ac73-a62a48fa8584',
    -- manila-v1: Slack config CRUD
    '475b0f6b-5fa8-4538-b5d4-1a1fd8f0edac',
    -- lincoln: test cleanup
    '7c129790-e748-46c2-8df9-33ba481e5926',
    -- amman-v1: Repo CRUD API
    'c3c6b2c6-91ca-4b18-853d-acca112bb88d',
    -- athens-v1: service contract pivot, frontend, specs, ADR-014
    '86f56dae-f2d1-4925-9432-d045748405ef',
    '2491e4ab-a8db-41ea-88b6-fa3ba7f79f4e',
    '0b77a432-3ad2-46d7-8050-92c585b8b373',
    '2ec2f4d2-3394-4e2f-921e-6375c52e89a8',
    'a4bdcfac-5d0c-4b95-8909-92a1982c8a67',
    -- montgomery: username auth
    '4a6ca482-1a05-4c13-a31c-3c4f95cf86fd',
    '21ed1539-e1cc-4497-a386-2d4b12d8cd96',
    -- delhi-v1: bulk endpoints (FastAPI)
    '49dad312-97d7-41de-856a-16016f46b23a',
    -- ev: tessera resume entry
    'a200dcbb-7201-4ecd-8bc9-243e35bfd2c9'
)
AND valid_to IS NULL;

-- ============================================================
-- Workspace names → mimir
-- ============================================================
UPDATE decisions
SET agent_context = jsonb_set(
    COALESCE(agent_context, '{}'::jsonb),
    '{client,project}',
    '"mimir"'::jsonb,
    true
)
WHERE id IN (
    -- san-francisco: GitHub→provider-agnostic field renaming, semgrep
    '74a5e390-8689-4bd0-ac4f-1ff2b6ffaf0e',
    '07aeb2a3-0d5c-4708-877e-1020e2cc50ca',
    '5e473d33-c058-4bc0-b0d7-a944b78c2734',
    'c6a19025-6e18-4807-a7bb-ca608d3ec9e7',
    -- san-antonio-v2: PR review for spec file renaming
    '518c4138-71ea-46c3-a6c3-bca9352c6e5f',
    -- abu-dhabi: PR review for spec genericization
    '74c0e842-af28-46cb-900c-3ebade7fc352'
)
AND valid_to IS NULL;

-- ============================================================
-- Workspace names → ashita-ai (blog posts)
-- ============================================================
UPDATE decisions
SET agent_context = jsonb_set(
    COALESCE(agent_context, '{}'::jsonb),
    '{client,project}',
    '"ashita-ai"'::jsonb,
    true
)
WHERE id IN (
    -- daegu-v2: blog post outlines, writing, editorial review
    '9ae85d4a-e903-4768-9c86-561608d785c8',
    'edbe0b6d-0c05-4270-8c12-ba8399b469df',
    'ffbf2edd-b1f1-48c7-a8e3-a1fe71606de9',
    '7c19a273-a196-4c97-926a-c1c164fe2192',
    'fabb40d5-10d9-4f41-9a44-778c36474bd2',
    '2eb8b6af-f4d5-4423-a867-fcd487863e30'
)
AND valid_to IS NULL;

-- ============================================================
-- Retract missoula decisions (commercial framing content, no longer relevant)
-- ============================================================
UPDATE decisions
SET valid_to = NOW()
WHERE id IN (
    '6c483e64-1456-4fd0-a93c-f2b66eb18e62',
    '96ab3ada-4d9f-4983-8947-11318cc80af6'
)
AND valid_to IS NULL;
