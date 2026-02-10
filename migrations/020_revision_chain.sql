-- Decision revision chain: links revised decisions to their predecessors.
ALTER TABLE decisions ADD COLUMN supersedes_id UUID REFERENCES decisions(id);
CREATE INDEX idx_decisions_supersedes ON decisions(supersedes_id) WHERE supersedes_id IS NOT NULL;

-- Tamper-evident content hash: SHA-256 of canonical decision fields.
ALTER TABLE decisions ADD COLUMN content_hash TEXT;
CREATE INDEX idx_decisions_content_hash ON decisions(content_hash) WHERE content_hash IS NOT NULL;

-- Merkle tree integrity proofs: periodic batch hashes for tamper detection.
CREATE TABLE integrity_proofs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    batch_start     TIMESTAMPTZ NOT NULL,
    batch_end       TIMESTAMPTZ NOT NULL,
    decision_count  INTEGER NOT NULL,
    root_hash       TEXT NOT NULL,
    previous_root   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_integrity_proofs_org_time ON integrity_proofs(org_id, created_at DESC);
