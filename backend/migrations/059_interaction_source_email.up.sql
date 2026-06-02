-- 059_interaction_source_email.up.sql
-- Add 'email' to interaction.source. See spec §6.2 (NOT migration-only: also
-- adds InteractionSourceEmail constant + updates the source-check test).
-- Existing set (after 055_phone_call): manual, gcal, todoist, telegram,
-- messages, anarlog_sessions, phone_calls.
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions', 'phone_calls', 'email'));
