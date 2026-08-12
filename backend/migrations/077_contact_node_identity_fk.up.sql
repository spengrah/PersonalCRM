-- Foreign key for the node/contact identity equation (contact.id == node.id).
--
-- Person nodes and contacts share an id, stated in 064_graph_identity.up.sql
-- and 068_backfill_person_nodes.up.sql and assumed by roughly a dozen Go call
-- sites, but nothing enforced it. ContactRepository.CreateContact now inserts
-- the node and the contact in one statement (contact.sql's
-- CreateContactWithNode CTE), so every contact has a matching node from here
-- on; this migration backfills the gap for existing rows and turns the
-- equation into a database constraint.
--
-- Wrapped in an explicit transaction, matching the shape of
-- backend/migrations/064_graph_identity.up.sql:9-13. This repo's golang-migrate
-- config sends each file as one multi-statement string via lib/pq's simple
-- query protocol, which Postgres already treats as an implicit transaction —
-- so BEGIN/COMMIT are redundant for atomicity today. They stay for explicit
-- self-documentation, matching the other self-wrapped migrations in this repo.
BEGIN;

-- Abort rather than silently accept a non-person node satisfying the FK,
-- BEFORE any backfill row is inserted. A foreign key cannot reference a
-- filtered subset of rows, so the constraint below requires SOME node, not a
-- person node specifically — this preflight is the one-time check that closes
-- the gap at migration time. It is not an ongoing guarantee: after this
-- migration, "every contact has a node" is a database invariant, while "that
-- node is a person" remains a repository convention held by
-- ContactRepository.CreateContact being the only insert path.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM contact c JOIN node n ON n.id = c.id WHERE n.type <> 'person') THEN
        RAISE EXCEPTION 'contact id collides with a non-person node; resolve before adding contact_id_node_fk';
    END IF;
END $$;

-- 068 backfilled only live contacts, so pre-068 tombstoned contacts have no
-- node and would fail FK validation. Mirror the tombstone, matching
-- ContactService.DeleteContact.
INSERT INTO node (id, type, canonical_label, created_at, deleted_at)
SELECT c.id, 'person', c.full_name, c.created_at, c.deleted_at
FROM contact c
WHERE NOT EXISTS (SELECT 1 FROM node n WHERE n.id = c.id)
ON CONFLICT (id) DO NOTHING;

-- No ON DELETE action (NO ACTION, the default): a node hard-delete must not
-- silently take a contact with it.
ALTER TABLE contact
    ADD CONSTRAINT contact_id_node_fk FOREIGN KEY (id) REFERENCES node (id);

COMMIT;
