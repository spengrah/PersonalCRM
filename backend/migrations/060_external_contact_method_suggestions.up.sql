-- 060_external_contact_method_suggestions.up.sql
-- Address-book contact-method gap + leak fix: durable storage for the
-- per-row suggestion/dismissal STATE the reconcile path produces for
-- already-linked address-book entries (gcontacts / icloud_contacts).
--
-- Two additive nullable JSONB columns:
--   pending_method_suggestions    the current un-applied missing-method
--                                 set for a linked `imported` row, shape
--                                 [{"type":"email","value":"<normalized>"}, ...].
--                                 NULL = no pending suggestions.
--   dismissed_method_suggestions  the append-only set of (type,value) the
--                                 user has dismissed; the reconcile path
--                                 and the suggestion list both subtract
--                                 these. NULL = nothing dismissed.
--
-- These columns are DELIBERATELY absent from UpsertExternalContact's
-- INSERT column list and DO UPDATE SET list, so an address-book producer
-- resync (which replaces `metadata` wholesale) never overwrites them.
-- That upsert-survival is the load-bearing reason this state does not
-- live in `metadata`. No backfill — NULL is the correct "none" default
-- for every existing row.
ALTER TABLE external_contact
    ADD COLUMN pending_method_suggestions JSONB NULL,
    ADD COLUMN dismissed_method_suggestions JSONB NULL;
