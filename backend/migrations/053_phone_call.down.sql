-- Down migration is destructive if rows exist. Guard all row-bearing drops
-- to prevent silently destroying staged-but-not-yet-interacted rows or
-- breaking referential integrity of existing phone_calls interactions.

-- 2. Revert interaction.source CHECK. Refuse if any phone_calls-source rows
--    exist — the constraint would fail anyway; raise early for a clear
--    error.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM interaction WHERE source = 'phone_calls') THEN
        RAISE EXCEPTION 'cannot revert interaction.source check: % rows still use phone_calls',
            (SELECT count(*) FROM interaction WHERE source = 'phone_calls');
    END IF;
END $$;
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages'));

-- 1. Drop phone_call — guard on row count first. If any rows exist
--    (processed or not), the operator must export them out-of-band before
--    rollback, otherwise the staging audit trail is destroyed silently.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM phone_call LIMIT 1) THEN
        RAISE EXCEPTION 'cannot drop phone_call: % rows present; export before rollback',
            (SELECT count(*) FROM phone_call);
    END IF;
END $$;
DROP INDEX IF EXISTS idx_phone_call_mac_host;
DROP INDEX IF EXISTS idx_phone_call_matched_contact;
DROP TABLE IF EXISTS phone_call;
