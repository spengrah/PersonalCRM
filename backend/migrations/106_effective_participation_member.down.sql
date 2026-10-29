-- Restores migration 089's views: effective_participation aggregates the
-- verdict view again, and the verdict view walks every node's alias chain.

BEGIN;

CREATE OR REPLACE VIEW effective_participation AS
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

DROP VIEW effective_participation_member;

CREATE OR REPLACE VIEW interaction_observation_verdict AS
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

COMMIT;
