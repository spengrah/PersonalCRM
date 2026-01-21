-- Migration: Move contact.notes to note table
-- This migration copies existing contact notes to the note table with category='notepad'
-- and then drops the notes column from the contact table.

-- Step 1: Copy non-empty, non-whitespace notes to the note table
-- Uses the contact's created_at and updated_at (or created_at if updated_at is null) for timestamps
INSERT INTO note (contact_id, body, category, created_at, updated_at)
SELECT
    id,
    notes,
    'notepad',
    created_at,
    COALESCE(updated_at, created_at)
FROM contact
WHERE notes IS NOT NULL
  AND TRIM(notes) != ''
  AND deleted_at IS NULL;

-- Step 2: Drop the notes column from the contact table
ALTER TABLE contact DROP COLUMN notes;
