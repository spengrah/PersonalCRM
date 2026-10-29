-- Effective participation, one row per member observation.
--
-- effective_participation (migration 089) packs each entry's members into
-- JSON, which a reader of every entry, such as the all-interactions list, pays
-- for on every row. effective_participation_member is the derivation itself,
-- one row per effective observation with its entry's key, its place among the
-- entry's members, and the entry's direction and time. effective_participation
-- becomes a packing of these rows, so the derivation stays in one place.
-- Neither view's rows change.
--
-- The verdict view also reads the root more cheaply: an observation's node is
-- its own root unless it was merged, and only a merged node's alias chain is
-- walked. The function is STRICT, so an unmerged node calls nothing.

BEGIN;

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
LEFT JOIN node n ON n.id = i.interaction_node_id
-- The join reads the root as the node row it names, which also lets the
-- generated code type root_node_id as nullable (no root for a burst).
LEFT JOIN node rn ON rn.id = COALESCE(interaction_node_root(n.merged_into), i.interaction_node_id)
WHERE i.deleted_at IS NULL;

-- effective_participation_member: one row per effective observation of a
-- contact. The contact's effective correctable observations group by their
-- root into one entry per root: mutual when the entry holds a mutual
-- observation or both an inbound and an outbound one, otherwise the entry's
-- shared direction, at the latest of the entry's times. Each message-burst
-- observation is an entry of its own with no root. entry_id keys the entry:
-- its root, or a burst observation's own id. member_ordinal orders an entry's
-- members by time, then observation.
-- The casts type the computed columns for the generated code.
CREATE VIEW effective_participation_member AS
SELECT
    e.contact_id,
    e.entry_id,
    e.root_node_id,
    e.observation_id,
    e.source,
    e.source_ref,
    e.interaction_node_id,
    e.occurred_at,
    (row_number() OVER entry)::int AS member_ordinal,
    CASE
        WHEN bool_or(e.direction = 'mutual') OVER whole_entry
          OR (bool_or(e.direction = 'inbound') OVER whole_entry AND bool_or(e.direction = 'outbound') OVER whole_entry)
        THEN 'mutual'
        ELSE min(e.direction) OVER whole_entry
    END::text AS entry_direction,
    (max(e.occurred_at) OVER whole_entry)::timestamptz AS entry_occurred_at
FROM (
    SELECT
        v.*,
        COALESCE(v.root_node_id, v.observation_id) AS entry_id
    FROM interaction_observation_verdict v
    WHERE NOT v.excluded
) e
-- entry_id determines root_node_id, so partitioning by the root too splits no
-- entry; it lets a reader's qual on the root reach the verdict view's scan.
WINDOW entry AS (
    PARTITION BY e.contact_id, e.entry_id, e.root_node_id
    ORDER BY e.occurred_at, e.observation_id
), whole_entry AS (entry ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING);

-- effective_participation: one row per effective participation of a contact,
-- packing effective_participation_member's rows of one entry. members lists
-- them in member order.
CREATE OR REPLACE VIEW effective_participation AS
SELECT
    m.contact_id,
    m.root_node_id AS interaction_node_id,
    min(m.entry_direction)::text AS direction,
    max(m.entry_occurred_at)::timestamptz AS occurred_at,
    jsonb_agg(
        jsonb_build_object(
            'observation_id', m.observation_id,
            'source', m.source,
            'source_ref', m.source_ref,
            'interaction_node_id', m.interaction_node_id
        )
        ORDER BY m.member_ordinal
    )::jsonb AS members
FROM effective_participation_member m
GROUP BY m.contact_id, m.entry_id, m.root_node_id;

COMMIT;
