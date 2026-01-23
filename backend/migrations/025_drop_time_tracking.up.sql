-- Drop time tracking schema
-- Migration: 025_drop_time_tracking

DROP INDEX IF EXISTS idx_time_entry_end_time;
DROP INDEX IF EXISTS idx_time_entry_project;
DROP INDEX IF EXISTS idx_time_entry_contact_id;
DROP INDEX IF EXISTS idx_time_entry_start_time;

DROP TABLE IF EXISTS time_entry;
