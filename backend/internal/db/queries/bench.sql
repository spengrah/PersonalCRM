-- Reads of the all-interactions view (the bench). Participation comes from the
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

-- name: ListThreadAnchorObservations :many
-- The live observations holding a copy of the thread's latest content at or
-- before the cut-off.
SELECT DISTINCT tc.observation_id::uuid AS observation_id
FROM thread_content tc
JOIN interaction i ON i.id = tc.observation_id AND i.deleted_at IS NULL
WHERE tc.venue_id = @venue_id
  AND tc.message_key = (
      SELECT latest.message_key
      FROM thread_content latest
      WHERE latest.venue_id = @venue_id
        AND latest.sent_at <= @cut_off
      ORDER BY latest.sent_at DESC, latest.message_key DESC
      LIMIT 1)
ORDER BY 1;

-- name: GetThreadMessage :one
-- One message of a thread by its key: its time, whether the user wrote it, its
-- text, and the contact a copy attributes it to.
SELECT sent_at, from_user, body, peer_contact_id
FROM thread_content
WHERE venue_id = @venue_id
  AND message_key = @message_key
ORDER BY peer_contact_id NULLS LAST
LIMIT 1;

-- name: ListVenueObservations :many
-- A venue's live observations, oldest first.
SELECT id, occurred_at
FROM interaction
WHERE venue_id = @venue_id
  AND deleted_at IS NULL
ORDER BY occurred_at, id;
