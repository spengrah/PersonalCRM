-- 060_external_contact_method_suggestions.down.sql
-- Reverses 060_external_contact_method_suggestions.up.sql. Dropping the
-- columns is safe: only the (inert in PR 1) reconcile path writes them
-- and the PR-2 suggestions surface reads them. No other reader exists.
ALTER TABLE external_contact
    DROP COLUMN IF EXISTS pending_method_suggestions,
    DROP COLUMN IF EXISTS dismissed_method_suggestions;
