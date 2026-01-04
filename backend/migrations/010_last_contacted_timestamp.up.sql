-- Change last_contacted from DATE to TIMESTAMPTZ to support minute-level cadences in testing mode
-- This preserves existing date values by converting them to timestamps at midnight UTC

ALTER TABLE contact
    ALTER COLUMN last_contacted TYPE TIMESTAMPTZ
    USING last_contacted::TIMESTAMPTZ;
