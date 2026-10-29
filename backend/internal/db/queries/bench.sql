-- Reads of the all-interactions list. Participation comes from the
-- one derivation, the effective_participation view of migration 089; content
-- comes from the one definition of a thread's content, the thread_content view
-- of migrations 091 and 096.

-- name: ListAllEffectiveParticipationMembers :many
-- Every live contact's effective participations, one row per member
-- observation. The repository combines direction and time while grouping
-- adjacent rows into entries, in the effective_participation view's member order.
SELECT
    v.contact_id,
    v.root_node_id,
    v.observation_id,
    v.source,
    v.source_ref,
    v.interaction_node_id,
    v.direction,
    v.occurred_at
FROM interaction_observation_verdict v
WHERE NOT v.excluded
  AND v.contact_id IN (SELECT id FROM live_contact)
ORDER BY v.contact_id, v.root_node_id, CASE WHEN v.root_node_id IS NULL THEN v.observation_id END, v.occurred_at, v.observation_id;

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
