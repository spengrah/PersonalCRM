-- external_sync_log.metadata was written once per sync run with a full copy of
-- external_sync_state.metadata and never read. For providers whose state
-- metadata holds large resolver caches (gchat), that copy dominated the
-- database: ~140 runs/day x ~300 KB of jsonb. The state row remains the sole
-- home of provider metadata; the log keeps only per-run counters and status.
--
-- DROP COLUMN does not reclaim the TOAST pages already written; run
-- VACUUM FULL external_sync_log once after this migration to return the space.
ALTER TABLE external_sync_log DROP COLUMN metadata;
