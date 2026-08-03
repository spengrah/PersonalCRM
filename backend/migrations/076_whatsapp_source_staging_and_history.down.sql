-- 076_whatsapp_source_staging_and_history.down.sql
-- Refuse-then-revert, mirroring 061_interaction_source_gchat.down.sql. Each
-- guard names state the revert would destroy and that nothing else can
-- reconstruct; a clean database passes all four and reverts fully.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM interaction WHERE source = 'whatsapp') THEN
        RAISE EXCEPTION 'cannot revert interaction.source check: % rows still use whatsapp',
            (SELECT count(*) FROM interaction WHERE source = 'whatsapp');
    END IF;

    IF EXISTS (SELECT 1 FROM comms_message WHERE matched_contact_id IS NULL) THEN
        RAISE EXCEPTION 'cannot restore comms_message.matched_contact_id NOT NULL: % rows have a NULL contact',
            (SELECT count(*) FROM comms_message WHERE matched_contact_id IS NULL);
    END IF;

    -- An outstanding chunk's media is still undeleted on WhatsApp's servers and
    -- the row is the only pointer to it. `failed` is included because a failed
    -- row is operator-requeueable (RequeueFailedNotification): dropping the
    -- table would destroy the only surviving pointer for a recoverable chunk.
    IF EXISTS (SELECT 1 FROM whatsapp_history_notification WHERE state IN ('pending', 'processing', 'failed')) THEN
        RAISE EXCEPTION 'cannot drop whatsapp_history_notification: % chunks are still outstanding (pending/processing/failed)',
            (SELECT count(*) FROM whatsapp_history_notification WHERE state IN ('pending', 'processing', 'failed'));
    END IF;

    -- Per-chat track/ignore overrides are user decisions and are not
    -- reconstructible from anything else.
    IF EXISTS (SELECT 1 FROM whatsapp_chat_config) THEN
        RAISE EXCEPTION 'cannot drop whatsapp_chat_config: % per-chat overrides would be lost',
            (SELECT count(*) FROM whatsapp_chat_config);
    END IF;
END $$;

DROP TABLE whatsapp_chat_config;
DROP TABLE whatsapp_history_notification;

-- Restore 073's eligible indexes verbatim (without the nullability predicate,
-- which is dead weight again once the column is NOT NULL).
DROP INDEX IF EXISTS idx_comms_message_stale_claim;
DROP INDEX IF EXISTS idx_comms_message_unprocessed_eligible;

CREATE INDEX idx_comms_message_unprocessed_eligible
    ON comms_message(source, matched_contact_id, sent_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NULL
      AND deleted_at IS NULL;

CREATE INDEX idx_comms_message_stale_claim
    ON comms_message(source, matched_contact_id, claimed_at)
    WHERE processed_at IS NULL
      AND claimed_at IS NOT NULL
      AND deleted_at IS NULL;

DROP INDEX IF EXISTS idx_comms_message_unmatched_peer;
DROP INDEX IF EXISTS idx_comms_message_dedup_unmatched;

ALTER TABLE comms_message DROP CONSTRAINT comms_message_contact_source_check;
ALTER TABLE comms_message ALTER COLUMN matched_contact_id SET NOT NULL;

COMMENT ON COLUMN comms_message.matched_contact_id IS NULL;

ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions', 'phone_calls', 'email', 'gchat'));
