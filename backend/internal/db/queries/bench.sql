-- Reads of the all-interactions list. Participation comes from the
-- one derivation, the effective_participation view of migration 089; content
-- comes from the one definition of a thread's content, the thread_content view
-- of migrations 091 and 096.

-- name: ListAllEffectiveParticipation :many
-- Every live contact's effective participations.
SELECT ep.* FROM effective_participation ep
WHERE ep.contact_id IN (SELECT id FROM live_contact)
ORDER BY ep.contact_id, ep.occurred_at DESC, ep.members->0->>'observation_id';

-- name: ListLiveObservationVenues :many
-- The venue of every live observation recorded in one.
SELECT id, venue_id::uuid AS venue_id
FROM interaction
WHERE deleted_at IS NULL
  AND venue_id IS NOT NULL;

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
