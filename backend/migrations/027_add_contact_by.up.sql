-- Migration: Add contact_by column for persistent due dates
-- This column stores the date by which a contact should be reached,
-- computed from (last_contacted || created_at) + cadence duration.

-- Step 1: Add the contact_by column (DATE, nullable)
ALTER TABLE contact ADD COLUMN contact_by DATE NULL;

-- Step 2: Create index for efficient overdue queries
CREATE INDEX idx_contact_contact_by ON contact(contact_by) WHERE contact_by IS NOT NULL AND deleted_at IS NULL;

-- Step 3: Backfill contact_by for existing contacts with cadence
-- Using fixed day counts as specified in the issue:
-- weekly: +7, biweekly: +14, monthly: +30, quarterly: +90, biannual: +180, annual: +365
UPDATE contact
SET contact_by = (
    COALESCE(last_contacted, created_at)::date +
    CASE cadence
        WHEN 'weekly' THEN 7
        WHEN 'biweekly' THEN 14
        WHEN 'monthly' THEN 30
        WHEN 'quarterly' THEN 90
        WHEN 'biannual' THEN 180
        WHEN 'annual' THEN 365
        ELSE 0
    END
)
WHERE cadence IS NOT NULL
  AND cadence != ''
  AND deleted_at IS NULL;
