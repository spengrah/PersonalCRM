-- Rollback: Restore contact.notes column and copy data back from note table

-- Step 1: Add the notes column back to the contact table
ALTER TABLE contact ADD COLUMN notes TEXT;

-- Step 2: Copy notepad notes back to the contact table
UPDATE contact c
SET notes = n.body
FROM note n
WHERE n.contact_id = c.id
  AND n.category = 'notepad';

-- Step 3: Delete the migrated notepad notes from the note table
DELETE FROM note WHERE category = 'notepad';
