-- 052_external_contact_host_id_and_last_content_hash.up.sql
-- Mac daemon: per-row host ownership + content hash captured from
-- external_contact.upserted event source_id (`<entity_uuid>@<sha256-hex>`).
-- Powers GET /api/v1/host/:id/sync/:source/known-ids — the daemon
-- uses the returned hash to construct deterministic delete
-- source_ids per the mac-daemon spec.
--
-- host_id is set on INSERT, and on UPDATE via
-- COALESCE(external_contact.host_id, EXCLUDED.host_id). Legacy NULL
-- rows (created before this migration, or by sources that don't set
-- host_id) are claimed by the first non-NULL emit; non-NULL
-- ownership is then preserved across all subsequent upserts.
--
-- Re-pair limitation: if a new Mac pairs onto the same Pi (after
-- revoking the previous one), rows already owned by the previous
-- host's non-NULL host_id are preserved unchanged — the new host's
-- /known-ids returns empty for those rows, the new host's full
-- resync emits upserts that dedup-absorb at the event-log layer, and
-- the new host never appears to "see" its contacts on the Pi. (Rows
-- that happen to be legacy NULL self-heal on first emit and do not
-- need intervention.) Operator workaround for the non-NULL case is
-- scripts/admin/reset_icloud_contacts.sh, which prompts for
-- confirmation and hard-deletes all icloud_contacts external_contact
-- and event-log rows. Single-host operation (same Mac, same Keychain,
-- same iCloud account) works correctly without intervention.
--
-- Both columns are nullable. Pre-existing rows
-- (gcontacts/gcal_attendee/telegram) backfill with NULL. The
-- `icloud_contacts` ingest path always writes non-NULL values
-- (enforced at the application layer; no Postgres CHECK constraint
-- because other sources legitimately leave these NULL).
ALTER TABLE external_contact
    ADD COLUMN host_id UUID NULL REFERENCES mac_host(id) ON DELETE SET NULL,
    ADD COLUMN last_content_hash TEXT NULL;

-- Partial index supporting GET /sync/:source/known-ids. Live rows
-- owned by a specific host, scoped per (host, source).
CREATE INDEX idx_external_contact_host_source_live
    ON external_contact (host_id, source)
    WHERE deleted_at IS NULL AND host_id IS NOT NULL;
