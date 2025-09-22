-- Remove biweekly from the cadence check constraint (rollback)
ALTER TABLE contact 
DROP CONSTRAINT contact_cadence_check;

ALTER TABLE contact 
ADD CONSTRAINT contact_cadence_check 
CHECK (cadence = ANY (ARRAY['weekly'::text, 'monthly'::text, 'quarterly'::text, 'biannual'::text, 'annual'::text]));

