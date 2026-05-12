-- Down migration is destructive if rows exist. Guard all row-bearing drops
-- to prevent silently destroying staged-but-not-yet-interacted rows on
-- rollback.

-- 3. Revert interaction.source CHECK. Refuse if any messages-source rows
--    exist — the constraint would fail anyway; raise early for a clear
--    error.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM interaction WHERE source = 'messages') THEN
        RAISE EXCEPTION 'cannot revert interaction.source check: % rows still use messages',
            (SELECT count(*) FROM interaction WHERE source = 'messages');
    END IF;
END $$;
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram'));

-- 2. Drop messages_message — guard on row count first. If any rows exist
--    (processed or not), the operator must export them out-of-band before
--    rollback, otherwise unprocessed staging backlog is destroyed silently.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM messages_message LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop messages_message: % rows present; export before rollback',
            (SELECT count(*) FROM messages_message);
    END IF;
END $$;
DROP INDEX IF EXISTS idx_messages_message_not_deleted;
DROP INDEX IF EXISTS idx_messages_message_sent_at;
DROP INDEX IF EXISTS idx_messages_message_chat_guid;
DROP INDEX IF EXISTS idx_messages_message_stale_claim;
DROP INDEX IF EXISTS idx_messages_message_unprocessed_eligible;
DROP TABLE IF EXISTS messages_message;

-- 1. Revert telegram_message claim columns + indexes.
DROP INDEX IF EXISTS idx_telegram_message_stale_claim;
DROP INDEX IF EXISTS idx_telegram_message_unprocessed_eligible;
-- Re-create the original unprocessed index (matches migration 032).
CREATE INDEX idx_telegram_message_unprocessed ON telegram_message(matched_contact_id, sent_at)
    WHERE processed_at IS NULL AND matched_contact_id IS NOT NULL;
ALTER TABLE telegram_message DROP COLUMN claimed_session_ref;
ALTER TABLE telegram_message DROP COLUMN claimed_at;
