-- Allow the 'abandoned' status on external_sync_log, used by the river-based
-- scheduler to mark pre-crash 'running' rows as abandoned when a retry starts
-- a fresh log. See .agents/spec/event-bus-foundation.md §3.6.
ALTER TABLE external_sync_log DROP CONSTRAINT external_sync_log_status_check;
ALTER TABLE external_sync_log ADD CONSTRAINT external_sync_log_status_check
    CHECK (status IN ('running', 'success', 'partial', 'error', 'abandoned'));
