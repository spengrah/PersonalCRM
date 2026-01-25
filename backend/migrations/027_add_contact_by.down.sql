-- Rollback: Remove contact_by column

DROP INDEX IF EXISTS idx_contact_contact_by;

ALTER TABLE contact DROP COLUMN contact_by;
