-- Unify email/email into single email type

ALTER TABLE contact_method
    DROP CONSTRAINT IF EXISTS contact_method_type_check;

-- Remove duplicates that would collide after type change
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY contact_id, value_normalized
               ORDER BY is_primary DESC, created_at ASC, id ASC
           ) AS rn
    FROM contact_method
    WHERE type IN ('email_personal', 'email_work')
)
DELETE FROM contact_method cm
USING ranked r
WHERE cm.id = r.id
  AND r.rn > 1;

UPDATE contact_method
SET type = 'email'
WHERE type IN ('email_personal', 'email_work');

-- Normalize email values after type change (same rule as before)
UPDATE contact_method
SET value_normalized = lower(btrim(value))
WHERE type = 'email';

ALTER TABLE contact_method
    ADD CONSTRAINT contact_method_type_check
    CHECK (type IN (
        'email',
        'phone',
        'telegram',
        'discord',
        'twitter',
        'signal',
        'gchat',
        'whatsapp'
    ));
