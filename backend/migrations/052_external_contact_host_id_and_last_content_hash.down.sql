-- 052_external_contact_host_id_and_last_content_hash.down.sql
-- Two guards: refuse to drop if any rows carry host_id OR
-- last_content_hash. Mirrors the data-loss-prevention pattern from
-- migration 050. Manual override requires DROP COLUMN ... CASCADE
-- with operator awareness of the loss.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM external_contact WHERE host_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot drop external_contact.host_id: % rows have non-NULL value',
            (SELECT count(*) FROM external_contact WHERE host_id IS NOT NULL);
    END IF;
    IF EXISTS (SELECT 1 FROM external_contact WHERE last_content_hash IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot drop external_contact.last_content_hash: % rows have non-NULL value (run a daemon full resync to re-derive after re-up)',
            (SELECT count(*) FROM external_contact WHERE last_content_hash IS NOT NULL);
    END IF;
END $$;

DROP INDEX IF EXISTS idx_external_contact_host_source_live;
ALTER TABLE external_contact
    DROP COLUMN IF EXISTS last_content_hash,
    DROP COLUMN IF EXISTS host_id;
