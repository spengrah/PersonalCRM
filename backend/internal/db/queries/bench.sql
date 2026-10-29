-- Reads of the all-interactions list. Participation comes from the
-- one derivation, the effective_participation_member view of migration 106;
-- content comes from the one definition of a thread's content, the
-- thread_content view of migrations 091 and 096.

-- name: ListAllEffectiveParticipationMembers :many
-- Every live contact's effective participations, one row per member
-- observation, with its entry's key, member place, direction and time.
SELECT
    m.contact_id,
    m.entry_id,
    m.root_node_id,
    m.observation_id,
    m.source,
    m.source_ref,
    m.interaction_node_id,
    m.member_ordinal,
    m.entry_direction,
    m.entry_occurred_at
FROM effective_participation_member m
WHERE m.contact_id IN (SELECT id FROM live_contact);

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
