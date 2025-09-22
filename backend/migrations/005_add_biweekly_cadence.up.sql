-- Add biweekly to the cadence check constraint
ALTER TABLE contact 
DROP CONSTRAINT contact_cadence_check;

ALTER TABLE contact 
ADD CONSTRAINT contact_cadence_check 
CHECK (cadence = ANY (ARRAY['weekly'::text, 'biweekly'::text, 'monthly'::text, 'quarterly'::text, 'biannual'::text, 'annual'::text]));

