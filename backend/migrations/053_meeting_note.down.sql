-- Reverse 053_meeting_note.up.sql

-- Restore interaction.source CHECK to its state after migration 049.
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages'));

-- Drop the meeting_note table (cascades the indexes).
DROP TABLE IF EXISTS meeting_note;
