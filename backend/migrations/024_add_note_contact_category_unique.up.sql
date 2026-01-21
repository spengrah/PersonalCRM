-- Add unique constraint on (contact_id) for notepad notes to prevent duplicates
-- This protects against race conditions when concurrent requests try to create the same notepad
-- Only applies to 'notepad' category - other categories can have multiple notes per contact

-- First, delete any duplicate notepad notes (keeping the most recently updated one)
DELETE FROM note n1
WHERE n1.category = 'notepad'
  AND EXISTS (
    SELECT 1 FROM note n2
    WHERE n2.contact_id = n1.contact_id
      AND n2.category = 'notepad'
      AND n2.id != n1.id
      AND (n2.updated_at > n1.updated_at OR (n2.updated_at = n1.updated_at AND n2.id > n1.id))
  );

-- Create unique index for notepad category only
CREATE UNIQUE INDEX idx_note_contact_notepad_unique
ON note (contact_id)
WHERE category = 'notepad';
