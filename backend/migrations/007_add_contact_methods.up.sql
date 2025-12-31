-- Add contact_method table and migrate existing email/phone data

CREATE TABLE contact_method (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    type TEXT NOT NULL CHECK (type IN (
        'email_personal',
        'email_work',
        'phone',
        'telegram',
        'discord',
        'twitter',
        'signal',
        'gchat'
    )),
    value TEXT NOT NULL,
    is_primary BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (contact_id, type)
);

CREATE UNIQUE INDEX idx_contact_method_primary
ON contact_method(contact_id)
WHERE is_primary = TRUE;

CREATE INDEX idx_contact_method_contact_id ON contact_method(contact_id);
CREATE INDEX idx_contact_method_full_text ON contact_method USING gin(to_tsvector('english', value));

CREATE TRIGGER update_contact_method_updated_at BEFORE UPDATE ON contact_method
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Migrate existing email/phone fields into contact_method
INSERT INTO contact_method (contact_id, type, value)
SELECT id, 'email_personal', btrim(email)
FROM contact
WHERE email IS NOT NULL AND btrim(email) <> '';

INSERT INTO contact_method (contact_id, type, value)
SELECT id, 'phone', btrim(phone)
FROM contact
WHERE phone IS NOT NULL AND btrim(phone) <> '';

-- Set primary method: email_personal first, otherwise phone
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY contact_id
               ORDER BY CASE type
                   WHEN 'email_personal' THEN 1
                   WHEN 'phone' THEN 2
                   ELSE 3
               END
           ) AS rn
    FROM contact_method
)
UPDATE contact_method cm
SET is_primary = TRUE
FROM ranked r
WHERE cm.id = r.id
  AND r.rn = 1;

-- Drop old contact email/phone columns and indexes
DROP INDEX IF EXISTS idx_contact_email;
DROP INDEX IF EXISTS idx_contact_full_text;

ALTER TABLE contact
    DROP COLUMN email,
    DROP COLUMN phone;

-- Recreate full-text index on name only
CREATE INDEX idx_contact_full_text ON contact USING gin(to_tsvector('english', full_name));
