-- Remove trigram index
DROP INDEX IF EXISTS idx_contact_fullname_trgm;

-- Note: Not dropping pg_trgm extension as other features might depend on it
-- DROP EXTENSION IF EXISTS pg_trgm;
