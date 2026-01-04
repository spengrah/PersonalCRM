-- Revert last_contacted from TIMESTAMPTZ back to DATE
-- This will lose time precision (keeps only the date part)

ALTER TABLE contact
    ALTER COLUMN last_contacted TYPE DATE
    USING last_contacted::DATE;
