-- 060_external_contact_method_suggestions.down.sql
-- Reverses 060_external_contact_method_suggestions.up.sql. Dropping the
-- columns is safe: only the reconcile path writes them and the
-- suggestions surface reads them. No other reader exists.
ALTER TABLE external_contact
    DROP COLUMN IF EXISTS pending_method_suggestions,
    DROP COLUMN IF EXISTS dismissed_method_suggestions;
