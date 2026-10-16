-- Thread content reads for the thread-context builder (internal/threadctx).
-- Every content read goes through the thread_content view (migration 091), so
-- a thread's context, its latest content time and the eligibility window agree.

-- name: ListThreadContent :many
-- Every stored copy of every message of one thread at or before the cut-off.
-- Copies of one message share message_key; the builder keeps each message once.
SELECT * FROM thread_content
WHERE venue_id = @venue_id
  AND sent_at <= @cut_off
ORDER BY sent_at, message_key, peer_contact_id;

-- name: GetLatestThreadContentAt :one
-- The time of the thread's latest content at or before as_of; no row when none.
SELECT sent_at
FROM thread_content
WHERE venue_id = @venue_id
  AND sent_at <= @as_of
ORDER BY sent_at DESC
LIMIT 1;

-- name: ListThreadsWithContentIn :many
-- The threads with content after @after, up to and including @up_to.
SELECT DISTINCT venue_id
FROM thread_content
WHERE sent_at > @after
  AND sent_at <= @up_to
ORDER BY venue_id;

-- name: ListVenueContactsAsOf :many
-- The live contacts with a live interaction in a venue at or before as_of: a
-- meeting's participants, which meeting notes do not name as contacts.
SELECT DISTINCT i.contact_id
FROM interaction i
JOIN live_contact lc ON lc.id = i.contact_id
WHERE i.venue_id = sqlc.arg(venue_id)::uuid
  AND i.deleted_at IS NULL
  AND i.occurred_at <= @as_of
ORDER BY i.contact_id;

-- name: ListContactNamesForMasking :many
-- Every name the CRM holds for each live contact: its own full name and the
-- names its linked address-book records carry.
SELECT lc.id AS contact_id, lc.full_name AS name
FROM live_contact lc
WHERE lc.full_name <> ''
UNION ALL
SELECT lc.id, n.name
FROM external_contact ec
JOIN live_contact lc ON lc.id = ec.crm_contact_id
CROSS JOIN LATERAL (VALUES (ec.display_name), (ec.first_name), (ec.last_name)) AS n(name)
WHERE ec.deleted_at IS NULL
  AND n.name IS NOT NULL
  AND n.name <> ''
ORDER BY 1, 2;
