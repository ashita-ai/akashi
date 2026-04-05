-- 097: Add missing foreign key from project_links.org_id to organizations.
-- Migration 063 created the project_links table with org_id but omitted the FK constraint,
-- allowing orphaned rows to survive organization deletion.

ALTER TABLE project_links
    ADD CONSTRAINT fk_project_links_org
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE
    NOT VALID;

-- Validate in a separate statement to avoid holding an ACCESS EXCLUSIVE lock
-- on organizations for the duration of a full-table scan.
ALTER TABLE project_links VALIDATE CONSTRAINT fk_project_links_org;
