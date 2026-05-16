-- 052_external_contact_host_id_and_last_content_hash.up.sql
-- Mac daemon: per-row host ownership + content hash captured from
-- external_contact.upserted event source_id (`<entity_uuid>@<sha256-hex>`).
-- Powers GET /api/v1/host/:id/sync/:source/known-ids — daemon uses
-- the returned hash to construct deterministic delete source_ids
-- per spec line 343.
--
-- host_id is set ONLY on first insert. The UpsertExternalContact
-- query does NOT include it in ON CONFLICT DO UPDATE SET, so the
-- ORIGINAL paired host's ownership persists across content updates.
--
-- Re-pair limitation (v1 known issue): if a new Mac pairs onto the
-- same Pi (after revoking the previous one), the previous host's rows
-- survive with their original host_id and the new host's /known-ids
-- returns empty. The new host's full resync emits upserts that
-- dedup-absorb at the event-log layer, so the new host never appears
-- to "see" its contacts on the Pi. Operator workaround is the script
-- scripts/admin/reset_icloud_contacts.sh, which prompts for
-- confirmation and hard-deletes all icloud_contacts external_contact
-- and event-log rows. Single-host operation (same Mac, same Keychain,
-- same iCloud account) works correctly without intervention. See plan
-- D-JC1 (post-Codex-r4 revision).
--
-- Both columns are nullable. Pre-existing rows
-- (gcontacts/gcal_attendee/telegram) backfill with NULL. The
-- `icloud_contacts` ingest path always writes non-NULL values
-- (application-level invariant; see plan D-JC6 + R13).
ALTER TABLE external_contact
    ADD COLUMN host_id UUID NULL REFERENCES mac_host(id) ON DELETE SET NULL,
    ADD COLUMN last_content_hash TEXT NULL;

-- Partial index supporting GET /sync/:source/known-ids. Live rows
-- owned by a specific host, scoped per (host, source).
CREATE INDEX idx_external_contact_host_source_live
    ON external_contact (host_id, source)
    WHERE deleted_at IS NULL AND host_id IS NOT NULL;
