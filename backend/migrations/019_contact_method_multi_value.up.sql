-- Allow multiple contact methods per type with normalized uniqueness

ALTER TABLE contact_method
    ADD COLUMN value_normalized TEXT;

UPDATE contact_method
SET value_normalized = CASE
    WHEN type IN ('email_personal', 'email_work', 'gchat') THEN lower(btrim(value))
    WHEN type IN ('telegram', 'twitter', 'discord') THEN lower(regexp_replace(btrim(value), '^@+', ''))
    WHEN type IN ('phone', 'signal', 'whatsapp') THEN
        CASE
            WHEN btrim(value) = '' THEN ''
            WHEN btrim(value) ~ '^\\+' THEN
                '+' || regexp_replace(btrim(value), '\\D', '', 'g')
            ELSE
                CASE
                    WHEN length(regexp_replace(btrim(value), '\\D', '', 'g')) = 10 THEN
                        '+1' || regexp_replace(btrim(value), '\\D', '', 'g')
                    WHEN length(regexp_replace(btrim(value), '\\D', '', 'g')) = 11
                        AND left(regexp_replace(btrim(value), '\\D', '', 'g'), 1) = '1' THEN
                        '+' || regexp_replace(btrim(value), '\\D', '', 'g')
                    ELSE
                        '+' || regexp_replace(btrim(value), '\\D', '', 'g')
                END
        END
    ELSE btrim(value)
END;

ALTER TABLE contact_method
    ALTER COLUMN value_normalized SET NOT NULL;

ALTER TABLE contact_method
    DROP CONSTRAINT IF EXISTS contact_method_contact_id_type_key;

DROP INDEX IF EXISTS idx_contact_method_unique;

CREATE UNIQUE INDEX idx_contact_method_unique_value
    ON contact_method(contact_id, type, value_normalized);

CREATE INDEX idx_contact_method_type_normalized
    ON contact_method(type, value_normalized);
