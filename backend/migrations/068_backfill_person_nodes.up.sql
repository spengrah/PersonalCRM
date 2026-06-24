-- Backfill one person node per existing non-deleted contact (graph foundation).
--
-- The contact→node dual-write (in ContactService) covers all FUTURE contacts;
-- this migration seeds the node registry for contacts that already exist. Each
-- person node lives at the contact's own id (node.id == contact.id) so every
-- assertion the write API later makes about an existing contact resolves its
-- subject node directly.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a file, and this is data DML, so it self-wraps to stay all-or-
-- nothing. Deleted contacts are intentionally skipped — their node tombstone is
-- a later soft-delete-propagation concern, and no assertion can reference a
-- deleted contact's node yet. ON CONFLICT (id) DO NOTHING keeps re-runs a no-op
-- and tolerates any person node already created by the live dual-write.

BEGIN;

INSERT INTO node (id, type, canonical_label, created_at)
SELECT id, 'person', full_name, created_at
FROM contact
WHERE deleted_at IS NULL
ON CONFLICT (id) DO NOTHING;

COMMIT;
