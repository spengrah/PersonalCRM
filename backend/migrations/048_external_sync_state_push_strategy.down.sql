-- Down migration: revert the strategy CHECK to the three-value form.
-- The guard ensures we do not drop the constraint while live data
-- still uses the 'push' value. Operators rolling back partial deploys
-- must first DELETE the push rows (typically by uninstalling the Mac
-- daemon via the admin UI, which cascades the rows).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM external_sync_state WHERE strategy = 'push') THEN
        RAISE EXCEPTION 'cannot revert push strategy: % rows still use it',
            (SELECT count(*) FROM external_sync_state WHERE strategy = 'push');
    END IF;
END $$;

ALTER TABLE external_sync_state DROP CONSTRAINT IF EXISTS external_sync_state_strategy_check;
ALTER TABLE external_sync_state ADD CONSTRAINT external_sync_state_strategy_check
    CHECK (strategy IN ('contact_driven', 'fetch_all', 'fetch_filtered'));
