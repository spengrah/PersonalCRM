-- Down migration is destructive if rows exist. Refuse to revert the CHECK if
-- any email-source interactions remain — the constraint would fail anyway;
-- raise early for a clear error.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM interaction WHERE source = 'email') THEN
        RAISE EXCEPTION 'cannot revert interaction.source check: % rows still use email',
            (SELECT count(*) FROM interaction WHERE source = 'email');
    END IF;
END $$;
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram', 'messages', 'anarlog_sessions', 'phone_calls'));
