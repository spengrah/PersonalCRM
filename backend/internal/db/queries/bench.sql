-- Reads of the all-interactions list. Participation comes from the
-- one derivation, the effective_participation view of migration 089; content
-- comes from the one definition of a thread's content, the thread_content view
-- of migrations 091 and 096.

-- name: ListAllEffectiveParticipationMembers :many
-- Every live contact's effective participations, one row per member
-- observation. The repository combines direction and time while grouping
-- adjacent rows into entries, in the effective_participation view's member order.
-- The root is the observation's own node unless that node was merged, and only
-- then does interaction_node_root walk the alias chain. Leaving the view's
-- root_node_id unread lets the planner drop the view's root join, which calls
-- the function for every observation with a node.
SELECT
    m.contact_id,
    m.root_node_id,
    m.observation_id,
    m.source,
    m.source_ref,
    m.interaction_node_id,
    m.direction,
    m.occurred_at
FROM (
    SELECT
        v.contact_id,
        COALESCE(interaction_node_root(n.merged_into), v.interaction_node_id) AS root_node_id,
        v.observation_id,
        v.source,
        v.source_ref,
        v.interaction_node_id,
        v.direction,
        v.occurred_at
    FROM interaction_observation_verdict v
    LEFT JOIN node n ON n.id = v.interaction_node_id
    WHERE NOT v.excluded
      AND v.contact_id IN (SELECT id FROM live_contact)
) m
ORDER BY m.contact_id, m.root_node_id, CASE WHEN m.root_node_id IS NULL THEN m.observation_id END, m.occurred_at, m.observation_id;

-- name: ListGroupChatMessageCopies :many
-- Each group-chat message key with every observation holding a copy of it: a
-- group message is stored once per member contact.
SELECT DISTINCT observation_id::uuid AS observation_id, message_key
FROM thread_content
WHERE venue_kind = 'group_chat'
  AND observation_id IS NOT NULL
ORDER BY message_key, observation_id;

-- name: GetThreadMessage :one
-- One message of a thread by its key: its time, whether the user wrote it, its
-- text, and the contact a copy attributes it to.
SELECT sent_at, from_user, body, peer_contact_id
FROM thread_content
WHERE venue_id = @venue_id
  AND message_key = @message_key
ORDER BY peer_contact_id NULLS LAST
LIMIT 1;
