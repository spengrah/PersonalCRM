-- Revert unified email type back to legacy email_personal/email_work types.
-- We cannot recover work vs personal intent, so default to email_personal.

UPDATE contact_method
SET type = 'email_personal'
WHERE type = 'email';

ALTER TABLE contact_method
    DROP CONSTRAINT IF EXISTS contact_method_type_check;

ALTER TABLE contact_method
    ADD CONSTRAINT contact_method_type_check
    CHECK (type IN (
        'email_personal',
        'email_work',
        'phone',
        'telegram',
        'discord',
        'twitter',
        'signal',
        'gchat',
        'whatsapp'
    ));
