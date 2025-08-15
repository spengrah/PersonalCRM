-- Personal CRM Initial Schema Rollback
-- Migration: 001_initial_schema

-- Drop triggers
DROP TRIGGER IF EXISTS update_note_updated_at ON note;
DROP TRIGGER IF EXISTS update_contact_updated_at ON contact;

-- Drop the update function
DROP FUNCTION IF EXISTS update_updated_at_column();

-- Drop indexes (most will be dropped automatically with tables, but explicit for clarity)
DROP INDEX IF EXISTS idx_contact_full_text;
DROP INDEX IF EXISTS idx_note_full_text;
DROP INDEX IF EXISTS idx_contact_tag_tag_id;
DROP INDEX IF EXISTS idx_contact_tag_contact_id;
DROP INDEX IF EXISTS idx_connection_contact_b;
DROP INDEX IF EXISTS idx_connection_contact_a;
DROP INDEX IF EXISTS idx_reminder_completed;
DROP INDEX IF EXISTS idx_reminder_due_date;
DROP INDEX IF EXISTS idx_reminder_contact_id;
DROP INDEX IF EXISTS idx_interaction_type;
DROP INDEX IF EXISTS idx_interaction_date;
DROP INDEX IF EXISTS idx_interaction_contact_id;
DROP INDEX IF EXISTS idx_note_category;
DROP INDEX IF EXISTS idx_note_created_at;
DROP INDEX IF EXISTS idx_note_contact_id;
DROP INDEX IF EXISTS idx_tag_name;
DROP INDEX IF EXISTS idx_contact_cadence;
DROP INDEX IF EXISTS idx_contact_last_contacted;
DROP INDEX IF EXISTS idx_contact_deleted_at;
DROP INDEX IF EXISTS idx_contact_email;
DROP INDEX IF EXISTS idx_contact_full_name;

-- Drop tables in reverse dependency order
DROP TABLE IF EXISTS prompt_query;
DROP TABLE IF EXISTS interaction_embedding;
DROP TABLE IF EXISTS note_embedding;
DROP TABLE IF EXISTS contact_summary;
DROP TABLE IF EXISTS connection;
DROP TABLE IF EXISTS reminder;
DROP TABLE IF EXISTS interaction;
DROP TABLE IF EXISTS note;
DROP TABLE IF EXISTS contact_tag;
DROP TABLE IF EXISTS tag;
DROP TABLE IF EXISTS contact;

-- Note: We don't drop extensions as they might be used by other applications
