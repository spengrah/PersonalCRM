-- 050_external_contact_deleted_at.up.sql
-- Mac daemon: tombstone semantics for external_contact via deleted_at.
-- Spec: .ai/spec/mac-daemon.md §2 iCloud Contacts (deletion handling),
-- §3 external_contact rules.
--
-- The column gives the inline external_contact.deleted handler a
-- soft-delete target; every existing read query against external_contact
-- is updated in the same PR to filter `deleted_at IS NULL`. Existing rows
-- (gcontacts, gcal_attendee, telegram) backfill with deleted_at = NULL
-- by column default.

ALTER TABLE external_contact ADD COLUMN deleted_at TIMESTAMPTZ;

-- Replace the existing unmatched-source partial index with a deleted_at-
-- gated variant. The new partial predicate adds AND deleted_at IS NULL so
-- tombstoned rows are excluded from the highest-traffic Import-UI query.
DROP INDEX IF EXISTS idx_external_contact_unmatched;
CREATE INDEX idx_external_contact_unmatched
    ON external_contact(source, match_status)
    WHERE match_status = 'unmatched'
      AND deleted_at IS NULL;

-- Partial index for the per-CRM-contact list query. Keeps
-- ListExternalContactsForCRMContact tight when (rare) tombstoned rows
-- accumulate.
CREATE INDEX idx_external_contact_live_crm_contact
    ON external_contact(crm_contact_id)
    WHERE deleted_at IS NULL
      AND crm_contact_id IS NOT NULL;
