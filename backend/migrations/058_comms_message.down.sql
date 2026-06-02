-- Down migration is destructive if rows exist. Guard the table drop on row
-- count so a rollback cannot silently destroy stored content (the durable
-- substrate for later AI use). The operator must export rows out-of-band first.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM comms_message LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop comms_message: % rows present; export before rollback',
            (SELECT count(*) FROM comms_message);
    END IF;
END $$;
DROP INDEX IF EXISTS idx_comms_message_not_deleted;
DROP INDEX IF EXISTS idx_comms_message_thread;
DROP INDEX IF EXISTS idx_comms_message_contact_sent;
DROP INDEX IF EXISTS idx_comms_message_dedup;
DROP TABLE IF EXISTS comms_message;
