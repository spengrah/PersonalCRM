-- Revert contact_method table and restore email/phone columns

ALTER TABLE contact
    ADD COLUMN email TEXT,
    ADD COLUMN phone TEXT;

-- Restore email and phone from contact_method data
UPDATE contact c
SET email = cm.value
FROM contact_method cm
WHERE cm.contact_id = c.id
  AND cm.type = 'email_personal';

UPDATE contact c
SET phone = cm.value
FROM contact_method cm
WHERE cm.contact_id = c.id
  AND cm.type = 'phone';

-- Drop contact_method table and related indexes
DROP INDEX IF EXISTS idx_contact_method_full_text;
DROP INDEX IF EXISTS idx_contact_method_contact_id;
DROP INDEX IF EXISTS idx_contact_method_primary;

DROP TRIGGER IF EXISTS update_contact_method_updated_at ON contact_method;
DROP TABLE IF EXISTS contact_method;

-- Restore contact indexes
DROP INDEX IF EXISTS idx_contact_full_text;
CREATE INDEX idx_contact_email ON contact(LOWER(email)) WHERE email IS NOT NULL;
CREATE INDEX idx_contact_full_text ON contact USING gin(to_tsvector('english', full_name || ' ' || COALESCE(email, '')));
