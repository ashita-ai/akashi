-- SQLite schema for Akashi local-lite mode.
-- Mirrors the Postgres schema with SQLite-compatible types:
--   UUID   → TEXT
--   JSONB  → TEXT (JSON)
--   TEXT[] → TEXT (JSON array)
--   vector → BLOB
--   TIMESTAMPTZ → TEXT (RFC 3339)

PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS organizations (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    plan       TEXT NOT NULL DEFAULT 'oss',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS agents (
    id           TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL,
    org_id       TEXT NOT NULL REFERENCES organizations(id),
    name         TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL DEFAULT 'agent',
    api_key_hash TEXT,
    tags         TEXT NOT NULL DEFAULT '[]',    -- JSON array
    metadata     TEXT NOT NULL DEFAULT '{}',    -- JSON object
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen    TEXT,
    UNIQUE(org_id, agent_id)
);

CREATE TABLE IF NOT EXISTS agent_runs (
    id            TEXT PRIMARY KEY,
    agent_id      TEXT NOT NULL,
    org_id        TEXT NOT NULL,
    trace_id      TEXT,
    parent_run_id TEXT REFERENCES agent_runs(id),
    status        TEXT NOT NULL DEFAULT 'running',
    started_at    TEXT NOT NULL,
    completed_at  TEXT,
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS decisions (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES agent_runs(id),
    agent_id           TEXT NOT NULL,
    org_id             TEXT NOT NULL,
    decision_type      TEXT NOT NULL,
    outcome            TEXT NOT NULL,
    confidence         REAL NOT NULL,
    reasoning          TEXT,
    embedding          BLOB,
    outcome_embedding  BLOB,
    metadata           TEXT NOT NULL DEFAULT '{}',
    completeness_score REAL NOT NULL DEFAULT 0,
    outcome_score REAL,
    precedent_ref      TEXT,
    precedent_reason   TEXT,
    supersedes_id      TEXT,
    content_hash       TEXT,
    valid_from         TEXT NOT NULL DEFAULT (datetime('now')),
    valid_to           TEXT,
    transaction_time   TEXT NOT NULL DEFAULT (datetime('now')),
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    session_id         TEXT,
    agent_context      TEXT,
    api_key_id         TEXT,
    tool               TEXT,
    model              TEXT,
    project            TEXT,
    claim_embeddings_failed_at TEXT,
    claim_embedding_attempts   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_decisions_org_valid
    ON decisions(org_id, valid_to);
CREATE INDEX IF NOT EXISTS idx_decisions_agent
    ON decisions(org_id, agent_id, valid_from DESC);
CREATE INDEX IF NOT EXISTS idx_decisions_type
    ON decisions(org_id, decision_type);
CREATE INDEX IF NOT EXISTS idx_decisions_supersedes
    ON decisions(supersedes_id) WHERE supersedes_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decisions_precedent_ref
    ON decisions(precedent_ref) WHERE precedent_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decisions_project
    ON decisions(org_id, project) WHERE project IS NOT NULL;

-- FTS5 virtual table for full-text search over decisions.
CREATE VIRTUAL TABLE IF NOT EXISTS decisions_fts USING fts5(
    outcome,
    reasoning,
    decision_type,
    content='decisions',
    content_rowid='rowid'
);

-- Triggers to keep FTS index in sync with the decisions table.
CREATE TRIGGER IF NOT EXISTS decisions_fts_insert AFTER INSERT ON decisions BEGIN
    INSERT INTO decisions_fts(rowid, outcome, reasoning, decision_type)
    VALUES (new.rowid, new.outcome, COALESCE(new.reasoning, ''), new.decision_type);
END;

CREATE TRIGGER IF NOT EXISTS decisions_fts_delete BEFORE DELETE ON decisions BEGIN
    INSERT INTO decisions_fts(decisions_fts, rowid, outcome, reasoning, decision_type)
    VALUES ('delete', old.rowid, old.outcome, COALESCE(old.reasoning, ''), old.decision_type);
END;

CREATE TRIGGER IF NOT EXISTS decisions_fts_update AFTER UPDATE ON decisions BEGIN
    INSERT INTO decisions_fts(decisions_fts, rowid, outcome, reasoning, decision_type)
    VALUES ('delete', old.rowid, old.outcome, COALESCE(old.reasoning, ''), old.decision_type);
    INSERT INTO decisions_fts(rowid, outcome, reasoning, decision_type)
    VALUES (new.rowid, new.outcome, COALESCE(new.reasoning, ''), new.decision_type);
END;

CREATE TABLE IF NOT EXISTS alternatives (
    id               TEXT PRIMARY KEY,
    decision_id      TEXT NOT NULL REFERENCES decisions(id),
    label            TEXT NOT NULL,
    score            REAL,
    selected         INTEGER NOT NULL DEFAULT 0,
    rejection_reason TEXT,
    metadata         TEXT NOT NULL DEFAULT '{}',
    created_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_alternatives_decision ON alternatives(decision_id);

CREATE TABLE IF NOT EXISTS evidence (
    id              TEXT PRIMARY KEY,
    decision_id     TEXT NOT NULL REFERENCES decisions(id),
    org_id          TEXT NOT NULL,
    source_type     TEXT NOT NULL,
    source_uri      TEXT,
    content         TEXT NOT NULL,
    relevance_score REAL,
    embedding       BLOB,
    metadata        TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_evidence_decision ON evidence(decision_id);

CREATE TABLE IF NOT EXISTS scored_conflicts (
    id                     TEXT PRIMARY KEY,
    conflict_kind          TEXT NOT NULL DEFAULT 'cross_agent',
    decision_a_id          TEXT NOT NULL,
    decision_b_id          TEXT NOT NULL,
    org_id                 TEXT NOT NULL,
    agent_a                TEXT NOT NULL,
    agent_b                TEXT NOT NULL,
    decision_type_a        TEXT NOT NULL DEFAULT '',
    decision_type_b        TEXT NOT NULL DEFAULT '',
    outcome_a              TEXT NOT NULL DEFAULT '',
    outcome_b              TEXT NOT NULL DEFAULT '',
    topic_similarity       REAL,
    outcome_divergence     REAL,
    significance           REAL,
    scoring_method         TEXT NOT NULL DEFAULT '',
    explanation            TEXT,
    detected_at            TEXT NOT NULL DEFAULT (datetime('now')),
    category               TEXT,
    severity               TEXT,
    status                 TEXT NOT NULL DEFAULT 'open',
    resolved_by            TEXT,
    resolved_at            TEXT,
    resolution_note        TEXT,
    relationship           TEXT,
    confidence_weight      REAL,
    temporal_decay         REAL,
    resolution_decision_id TEXT,
    winning_decision_id    TEXT,
    group_id               TEXT,
    UNIQUE(decision_a_id, decision_b_id)
);
CREATE INDEX IF NOT EXISTS idx_conflicts_org ON scored_conflicts(org_id, status);

CREATE TABLE IF NOT EXISTS conflict_groups (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL,
    agent_a           TEXT NOT NULL,
    agent_b           TEXT NOT NULL,
    conflict_kind     TEXT NOT NULL DEFAULT 'cross_agent',
    decision_type     TEXT NOT NULL DEFAULT '',
    first_detected_at TEXT NOT NULL DEFAULT (datetime('now')),
    last_detected_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(org_id, agent_a, agent_b, conflict_kind, decision_type)
);

CREATE TABLE IF NOT EXISTS decision_assessments (
    id                TEXT PRIMARY KEY,
    decision_id       TEXT NOT NULL,
    org_id            TEXT NOT NULL,
    assessor_agent_id TEXT NOT NULL,
    outcome           TEXT NOT NULL,
    notes             TEXT,
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_assessments_decision ON decision_assessments(decision_id, org_id);

CREATE TABLE IF NOT EXISTS decision_claims (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    decision_id TEXT NOT NULL,
    org_id      TEXT NOT NULL,
    claim_idx   INTEGER NOT NULL,
    claim_text  TEXT NOT NULL,
    category    TEXT,
    embedding   BLOB
);
CREATE INDEX IF NOT EXISTS idx_claims_decision ON decision_claims(decision_id, org_id);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    org_id          TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    endpoint        TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'in_progress',
    status_code     INTEGER,
    response_data   TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (org_id, agent_id, endpoint, idempotency_key)
);

CREATE TABLE IF NOT EXISTS access_grants (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL,
    grantor_id    TEXT NOT NULL,
    grantee_id    TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT,
    permission    TEXT NOT NULL,
    granted_at    TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at    TEXT
);

CREATE TABLE IF NOT EXISTS api_keys (
    id           TEXT PRIMARY KEY,
    prefix       TEXT NOT NULL,
    key_hash     TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    org_id       TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at TEXT,
    expires_at   TEXT,
    revoked_at   TEXT
);

CREATE TABLE IF NOT EXISTS mutation_audit_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id     TEXT,
    org_id         TEXT NOT NULL,
    actor_agent_id TEXT,
    actor_role     TEXT,
    http_method    TEXT,
    endpoint       TEXT,
    operation      TEXT,
    resource_type  TEXT,
    resource_id    TEXT,
    before_data    TEXT,
    after_data     TEXT,
    metadata       TEXT,
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
