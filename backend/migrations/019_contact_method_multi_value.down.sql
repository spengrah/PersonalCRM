-- Revert multiple contact methods per type changes

DROP INDEX IF EXISTS idx_contact_method_type_normalized;
DROP INDEX IF EXISTS idx_contact_method_unique_value;

ALTER TABLE contact_method
    ADD CONSTRAINT contact_method_contact_id_type_key UNIQUE (contact_id, type);

ALTER TABLE contact_method
    DROP COLUMN IF EXISTS value_normalized;
