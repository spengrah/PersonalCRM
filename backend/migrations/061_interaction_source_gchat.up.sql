-- 061_interaction_source_gchat.up.sql
-- Add 'gchat' to interaction.source. See spec §6.1 (NOT migration-only: also
-- adds InteractionSourceGChat constant + updates the source-check test).
-- Also broadens the comms_message.thread_id column comment to document that
-- chat sources reuse it as the space/chat scope resource name (spec §6.2).
-- Existing set (after 059_interaction_source_email): manual, gcal, todoist,
-- telegram, messages, anarlog_sessions, phone_calls, email.
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions', 'phone_calls', 'email', 'gchat'));

COMMENT ON COLUMN comms_message.thread_id IS
    'email: Gmail threadId; gchat/telegram/messages: space/chat scope resource name';
