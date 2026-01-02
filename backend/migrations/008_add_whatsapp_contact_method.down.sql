-- Remove whatsapp from contact_method type constraint

-- First delete any whatsapp entries (they won't be valid after this migration reverts)
DELETE FROM contact_method WHERE type = 'whatsapp';

ALTER TABLE contact_method
DROP CONSTRAINT contact_method_type_check;

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
    'gchat'
));
