-- Rollback external sync infrastructure

-- Drop indexes first
DROP INDEX IF EXISTS idx_external_sync_log_status;
DROP INDEX IF EXISTS idx_external_sync_log_started_at;
DROP INDEX IF EXISTS idx_external_sync_log_source;
DROP INDEX IF EXISTS idx_external_sync_log_sync_state_id;

DROP INDEX IF EXISTS idx_external_sync_state_enabled;
DROP INDEX IF EXISTS idx_external_sync_state_next_sync;
DROP INDEX IF EXISTS idx_external_sync_state_status;
DROP INDEX IF EXISTS idx_external_sync_state_source;
DROP INDEX IF EXISTS idx_external_sync_state_source_account;

-- Drop tables
DROP TABLE IF EXISTS external_sync_log;
DROP TABLE IF EXISTS external_sync_state;
