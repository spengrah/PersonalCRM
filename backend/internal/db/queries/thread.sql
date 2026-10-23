-- Reads of the Threads tab. A thread is a venue holding content; its content
-- comes from the one definition of a thread's content, the thread_content view
-- (migrations 091 and 096), and its participants from the effective
-- observations recorded in the venue (migration 089), so a participant the
-- user removed from an interaction is no participant of its thread.

-- name: ListThreads :many
-- Every thread holding content, newest latest message first with the venue ID
-- as the tie-breaker. sources keeps the venues of those sources (all when
-- empty); contact_id keeps the threads that contact takes part in; with
-- only_ids set, only the threads venue_ids names.
SELECT t.venue_id, v.kind, v.source, v.title, t.message_count, t.latest_at
FROM (
    SELECT tc.venue_id,
           count(DISTINCT tc.message_key)::int AS message_count,
           max(tc.sent_at)::timestamptz AS latest_at
    FROM thread_content tc
    GROUP BY tc.venue_id
) t
JOIN venue v ON v.node_id = t.venue_id
WHERE (cardinality(sqlc.arg('sources')::text[]) = 0 OR v.source = ANY(sqlc.arg('sources')::text[]))
  AND (NOT sqlc.arg('only_ids')::boolean OR t.venue_id = ANY(sqlc.arg('venue_ids')::uuid[]))
  AND (sqlc.narg('contact_id')::uuid IS NULL OR EXISTS (
        SELECT 1
        FROM interaction i
        JOIN interaction_observation_verdict ov ON ov.observation_id = i.id AND NOT ov.excluded
        WHERE i.venue_id = t.venue_id
          AND i.contact_id = sqlc.narg('contact_id')::uuid))
ORDER BY t.latest_at DESC, t.venue_id DESC;

-- name: ListThreadParticipants :many
-- The live contacts that take part in each given thread, by name.
SELECT DISTINCT i.venue_id::uuid AS venue_id, lc.id AS participant_id, lc.full_name AS participant_name
FROM interaction i
JOIN interaction_observation_verdict ov ON ov.observation_id = i.id AND NOT ov.excluded
JOIN live_contact lc ON lc.id = i.contact_id
WHERE i.venue_id = ANY(sqlc.arg('venue_ids')::uuid[])
ORDER BY venue_id, participant_name, participant_id;

-- name: ListThreadMessageKeys :many
-- Each message of the given threads once, in thread order: by time, then key.
SELECT DISTINCT venue_id, message_key, sent_at
FROM thread_content
WHERE venue_id = ANY(sqlc.arg('venue_ids')::uuid[])
ORDER BY venue_id, sent_at, message_key;

-- name: GetThread :one
-- One thread holding content, as ListThreads names it.
SELECT t.venue_id, v.kind, v.source, v.title, t.message_count, t.latest_at
FROM (
    SELECT tc.venue_id,
           count(DISTINCT tc.message_key)::int AS message_count,
           max(tc.sent_at)::timestamptz AS latest_at
    FROM thread_content tc
    WHERE tc.venue_id = sqlc.arg('venue_id')::uuid
    GROUP BY tc.venue_id
) t
JOIN venue v ON v.node_id = t.venue_id;

-- name: ThreadHoldsMessage :one
SELECT EXISTS (
    SELECT 1 FROM thread_content
    WHERE venue_id = sqlc.arg('venue_id')::uuid AND message_key = sqlc.arg('message_key')::text);

-- name: ListThreadQuotedHistories :many
-- The quoted history of each email of one thread that stores one, by its
-- thread_content message_key, read once however many per-contact copies hold
-- the email. Only the thread read reads it: the thread_content view carries no
-- quoted history, so no decision-model context holds one.
SELECT DISTINCT ON (cm.external_id)
       (cm.source || ':' || cm.external_id)::text AS message_key,
       cm.quoted_history::text AS quoted_history
FROM comms_message cm
JOIN venue v
  ON v.source = cm.source
 AND v.source_container_id = cm.thread_id
WHERE v.node_id = @venue_id
  AND cm.source = 'email'
  AND cm.quoted_history IS NOT NULL
  AND cm.deleted_at IS NULL
ORDER BY cm.external_id, cm.id;
