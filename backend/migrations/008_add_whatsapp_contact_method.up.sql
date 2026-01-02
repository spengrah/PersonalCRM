-- Add whatsapp to contact_method type constraint

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
    'gchat',
    'whatsapp'
));
