-- Extend external_sync_state.strategy CHECK to include 'push' for Mac
-- daemon push providers (Messages, iCloud Contacts; readers ship in
-- later PRs). Push providers are NOT polled by the scheduler — the
-- service layer skips them via provider.Config().Strategy.
ALTER TABLE external_sync_state DROP CONSTRAINT IF EXISTS external_sync_state_strategy_check;
ALTER TABLE external_sync_state ADD CONSTRAINT external_sync_state_strategy_check
    CHECK (strategy IN ('contact_driven', 'fetch_all', 'fetch_filtered', 'push'));
