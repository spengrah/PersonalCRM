-- Identity backfill: gives stored observations their interaction nodes.
--
-- Gives every stored live observation of a correctable source its interaction
-- node, then makes the missing-node state unrepresentable. Every identity key
-- comes from resolve_interaction_node (migration 087), the one implementation of
-- the per-source identity rule that the live observation step also calls, so
-- this file derives no key of its own. Its one rule of its own is the fallback
-- below for a live gcal observation whose calendar_event row no longer exists.
--
-- Only interaction_node_id changes on interaction rows. Soft-deleted
-- observations and message bursts are left as they are: a soft-deleted
-- observation is not effective, and resolves a node when a source revives it.
--
-- Idempotent: every UPDATE is scoped to rows with no node, so a re-run creates
-- no node and changes no observation.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so this multi-statement backfill + DDL self-wraps
-- to stay all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

-- Fallback: a live gcal observation whose calendar_event row is gone has no
-- identity key (interaction_identity_key returns NULL), and nothing re-processes
-- it, so no later lookup needs a key. Its observations share one keyless node per
-- source_ref. Runs before the general backfill, which would otherwise give each
-- of them a node of its own.
CREATE TEMP TABLE gcal_orphan_node ON COMMIT DROP AS
SELECT source_ref, uuid_generate_v4() AS node_id
FROM (
    SELECT DISTINCT source_ref
    FROM interaction
    WHERE source = 'gcal'
      AND source_ref IS NOT NULL
      AND deleted_at IS NULL
      AND interaction_node_id IS NULL
      AND interaction_identity_key(source, source_ref) IS NULL
) orphaned;

INSERT INTO node (id, type, canonical_label)
SELECT node_id, 'interaction', '' FROM gcal_orphan_node;

INSERT INTO interaction_node (node_id, source, identity_key)
SELECT node_id, 'gcal', NULL FROM gcal_orphan_node;

UPDATE interaction i
SET interaction_node_id = g.node_id
FROM gcal_orphan_node g
WHERE i.source = 'gcal'
  AND i.source_ref = g.source_ref
  AND i.deleted_at IS NULL
  AND i.interaction_node_id IS NULL;

-- Every other live observation of a correctable source resolves through the
-- per-source identity rule.
UPDATE interaction i
SET interaction_node_id = (SELECT resolve_interaction_node(i.source, i.source_ref))
WHERE i.deleted_at IS NULL
  AND i.interaction_node_id IS NULL
  AND NOT interaction_source_is_burst(i.source);

-- Representability: a live observation of a correctable source has a node, and
-- an observation of a burst source never has one. A soft-deleted observation of
-- a correctable source may have none (it resolves when a source revives it).
ALTER TABLE interaction ADD CONSTRAINT interaction_node_representable_check
    CHECK (
        CASE WHEN interaction_source_is_burst(source)
             THEN interaction_node_id IS NULL
             ELSE deleted_at IS NOT NULL OR interaction_node_id IS NOT NULL
        END
    );

COMMIT;
