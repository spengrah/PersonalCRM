-- Add unique constraint on (contact_id, category) to prevent duplicate notes per category
-- This protects against race conditions when concurrent requests try to create the same note

-- First, delete any duplicate notepad notes (keeping the most recently updated one)
DELETE FROM note n1
WHERE n1.category IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM note n2
    WHERE n2.contact_id = n1.contact_id
      AND n2.category = n1.category
      AND n2.id != n1.id
      AND (n2.updated_at > n1.updated_at OR (n2.updated_at = n1.updated_at AND n2.id > n1.id))
  );

-- Create unique index for non-null categories
CREATE UNIQUE INDEX idx_note_contact_category_unique
ON note (contact_id, category)
WHERE category IS NOT NULL;
