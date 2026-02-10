-- Rename the token column to token_hash to reflect that only SHA-256 hashes
-- are stored, not raw verification tokens. Any existing plaintext tokens become
-- invalid (acceptable for pre-launch; no orgs are stuck mid-verification in prod).

ALTER TABLE email_verifications RENAME COLUMN token TO token_hash;
