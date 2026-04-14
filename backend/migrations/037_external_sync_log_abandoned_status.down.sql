-- Safety: coerce any 'abandoned' rows back to 'error' before re-tightening
-- the CHECK constraint, so the DOWN migration does not fail on legacy rows.
UPDATE external_sync_log
SET status = 'error',
    error_message = COALESCE(error_message, 'coerced from abandoned on down-migration')
WHERE status = 'abandoned';
ALTER TABLE external_sync_log DROP CONSTRAINT external_sync_log_status_check;
ALTER TABLE external_sync_log ADD CONSTRAINT external_sync_log_status_check
    CHECK (status IN ('running', 'success', 'partial', 'error'));
