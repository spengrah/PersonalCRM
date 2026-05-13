-- 050_external_contact_deleted_at.down.sql

-- Defensive: refuse to drop the column if any rows are tombstoned. A
-- tombstoned row becoming live on column drop would be a silent data
-- corruption (rows re-appear in import UI / dedup queries).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM external_contact WHERE deleted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot drop deleted_at: % tombstoned rows still exist',
            (SELECT count(*) FROM external_contact WHERE deleted_at IS NOT NULL);
    END IF;
END $$;

DROP INDEX IF EXISTS idx_external_contact_live_crm_contact;
DROP INDEX IF EXISTS idx_external_contact_unmatched;

-- Restore the original (pre-050) partial index from migration 014.
CREATE INDEX idx_external_contact_unmatched
    ON external_contact(source, match_status)
    WHERE match_status = 'unmatched';

ALTER TABLE external_contact DROP COLUMN deleted_at;
