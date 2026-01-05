-- Enable pg_trgm extension for fuzzy text matching
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Add trigram index on contact full_name for fast similarity searches
CREATE INDEX IF NOT EXISTS idx_contact_fullname_trgm ON contact USING gin (full_name gin_trgm_ops);
