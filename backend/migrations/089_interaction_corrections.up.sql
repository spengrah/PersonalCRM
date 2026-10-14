-- Interaction corrections and effective participation.
--
-- The user's corrections of interactions are SP1 state: assertions under three
-- correction predicates, each carrying user provenance, plus the registry
-- merge alias (node.merged_into) of an interaction node merged into another.
-- This migration marks the correction predicates in the catalog, seeds them, and
-- defines the ONE derivation of effective participation as SQL read over
-- observation rows, merge aliases and accepted correction assertions. Nothing
-- stores effective participation; every read computes it on demand, so a read
-- inside a transaction sees that transaction's own writes.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

-- predicate.correction marks the predicates whose assertions are the user's
-- corrections of interactions. It is the one source of that set: a read that
-- must treat corrections apart from other assertions derives the set from it.
ALTER TABLE predicate ADD COLUMN correction BOOLEAN NOT NULL DEFAULT FALSE;

-- The correction predicates. 'interaction' as a subject or object type names an
-- interaction node (migration 087). Permanent, with no valid-time bucket: a
-- correction is a judgment about what happened, not a fact with a lifetime.
--   not_participant_in    the person did not take part in the judged interaction
--                         node (a removal of named participants)
--   removed_for_everyone  value true: the judged interaction node is removed for
--                         every participant, present and later-observed
--   same_interaction_as   the user's merge declaration: the merged-away root is
--                         the same interaction as the root it was merged into
INSERT INTO predicate (
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    temporal_profile, default_review_policy, proposition_bucket, status,
    description, correction
) VALUES
    ('not_participant_in',   'edge', 'person',      'interaction', NULL,   'multi',  FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'The user removed this person from the judged interaction', TRUE),
    ('removed_for_everyone', 'fact', 'interaction', NULL,          'bool', 'single', FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'The user removed the judged interaction for every participant', TRUE),
    ('same_interaction_as',  'edge', 'interaction', 'interaction', NULL,   'multi',  FALSE,
     'permanent', 'auto-if-confident', 'none', 'curated',
     'The user declared this interaction the same interaction as another', TRUE);

-- The observations of an interaction node are read by node.
CREATE INDEX idx_interaction_interaction_node ON interaction (interaction_node_id)
    WHERE interaction_node_id IS NOT NULL;

-- interaction_node_root: the root of a node's merge-alias chain (the node
-- itself when it was never merged); NULL for a NULL or unknown node. An
-- interaction-node merge aliases one root to a distinct root and never rewrites
-- an alias, so every chain ends at a root.
CREATE FUNCTION interaction_node_root(p_node_id UUID)
RETURNS UUID
LANGUAGE sql
STABLE
STRICT
AS $$
    WITH RECURSIVE chain (id, merged_into) AS (
        SELECT n.id, n.merged_into FROM node n WHERE n.id = p_node_id
      UNION ALL
        SELECT n.id, n.merged_into FROM chain c JOIN node n ON n.id = c.merged_into
    )
    SELECT id FROM chain WHERE merged_into IS NULL;
$$;

-- interaction_alias_tree: every node of the alias tree under a root, the root
-- included: the members of a merged interaction.
CREATE FUNCTION interaction_alias_tree(p_root_id UUID)
RETURNS SETOF UUID
LANGUAGE sql
STABLE
STRICT
AS $$
    WITH RECURSIVE tree (id) AS (
        SELECT n.id FROM node n WHERE n.id = p_root_id
      UNION ALL
        SELECT n.id FROM tree t JOIN node n ON n.merged_into = t.id
    )
    SELECT id FROM tree;
$$;

-- interaction_observation_verdict: every live observation with the root of its
-- own interaction node and whether a correction excludes it. A correctable
-- observation is excluded by an accepted not_participant_in from the contact's
-- person node (whose id is the contact's id) to the observation's own node, or
-- by an accepted removed_for_everyone (true) on that node. A correction binds to
-- the node it judged, never to the node's root, so a merge neither hides nor
-- widens it. A message-burst observation has no node: never excluded, no root.
CREATE VIEW interaction_observation_verdict AS
SELECT
    i.id AS observation_id,
    i.contact_id,
    i.source,
    i.source_ref,
    i.direction,
    i.occurred_at,
    i.interaction_node_id,
    rn.id AS root_node_id,
    (
        EXISTS (
            SELECT 1 FROM assertion a
            WHERE a.predicate_key = 'not_participant_in'
              AND a.status = 'accepted'
              AND a.subject_node_id = i.contact_id
              AND a.object_node_id = i.interaction_node_id
        )
        OR EXISTS (
            SELECT 1 FROM assertion a
            WHERE a.predicate_key = 'removed_for_everyone'
              AND a.status = 'accepted'
              AND a.subject_node_id = i.interaction_node_id
              AND a.value_bool
        )
    )::boolean AS excluded
FROM interaction i
-- The join reads the root as the node row it names, which also lets the
-- generated code type root_node_id as nullable (no root for a burst).
LEFT JOIN node rn ON rn.id = interaction_node_root(i.interaction_node_id)
WHERE i.deleted_at IS NULL;

-- effective_participation: one row per effective participation of a contact.
-- The contact's effective correctable observations group by their root, one
-- entry per root: mutual when the group holds a mutual observation or both an
-- inbound and an outbound one, otherwise the group's shared direction, at the
-- latest of the group's times. Each message-burst observation is an entry of
-- its own with no interaction node. members lists each grouped observation.
-- The casts type the computed columns for the generated code.
CREATE VIEW effective_participation AS
SELECT
    v.contact_id,
    v.root_node_id AS interaction_node_id,
    CASE
        WHEN bool_or(v.direction = 'mutual')
          OR (bool_or(v.direction = 'inbound') AND bool_or(v.direction = 'outbound'))
        THEN 'mutual'
        ELSE min(v.direction)
    END::text AS direction,
    max(v.occurred_at)::timestamptz AS occurred_at,
    jsonb_agg(
        jsonb_build_object(
            'observation_id', v.observation_id,
            'source', v.source,
            'source_ref', v.source_ref,
            'interaction_node_id', v.interaction_node_id
        )
        ORDER BY v.occurred_at, v.observation_id
    )::jsonb AS members
FROM interaction_observation_verdict v
WHERE NOT v.excluded
GROUP BY v.contact_id, v.root_node_id, CASE WHEN v.root_node_id IS NULL THEN v.observation_id END;

COMMIT;
