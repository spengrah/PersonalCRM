DROP INDEX IF EXISTS idx_contact_last_response_at;
DROP INDEX IF EXISTS idx_contact_last_outreach_at;
DROP INDEX IF EXISTS idx_contact_last_interaction_at;
ALTER TABLE contact DROP COLUMN IF EXISTS last_response_at;
ALTER TABLE contact DROP COLUMN IF EXISTS last_outreach_at;
ALTER TABLE contact DROP COLUMN IF EXISTS last_interaction_at;
ALTER TABLE interaction DROP CONSTRAINT IF EXISTS interaction_direction_check;
ALTER TABLE interaction DROP COLUMN IF EXISTS direction;
ALTER TABLE interaction DROP CONSTRAINT IF EXISTS interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist'));
