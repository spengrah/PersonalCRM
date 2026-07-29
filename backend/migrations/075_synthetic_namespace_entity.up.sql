BEGIN;

-- synthetic_namespace_entity records which rows a synthetic seeding namespace
-- created, so a LATER, SEPARATE request can find them again.
--
-- Why it has to exist. The declared-seed endpoint seeds a namespace in one
-- request and cleans it up in another, so cleanup cannot use the harness's
-- in-memory id ledger — it died with the seeding request. Every other id set is
-- recovered from a generator-derived token carried by the row itself
-- ('synth-<ns>-…' names, source ids, hostnames). For contacts that recovery is
-- unsound: full_name is USER-EDITABLE, and node.canonical_label is rewritten
-- along with it, so a test that renames a seeded contact through the ordinary
-- contact API makes it invisible to both name-derived sweeps. Cleanup would
-- then skip the contact and everything hanging off it, delete the namespace's
-- discovery marker, and report success over live residue.
--
-- Ownership is therefore written once, at seed time, keyed by a column nothing
-- in the application can rewrite: the entity's own id. Rows are removed by the
-- cleanup ladder only after every other step succeeded, so a partially failed
-- sweep leaves the namespace both discoverable and recoverable.
--
-- No FK to the owned rows: the point is to survive whatever happens to them
-- (including a hard delete performed by cleanup itself), and a cascade would
-- delete the record before the sweep that reads it.
CREATE TABLE synthetic_namespace_entity (
    namespace   text        NOT NULL,
    entity_kind text        NOT NULL,
    entity_id   uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (namespace, entity_kind, entity_id)
);

COMMIT;
