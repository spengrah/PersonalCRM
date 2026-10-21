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

-- name: ListDisplayNamesForMasking :many
-- Every display name a stored message gives a sender or recipient, of every
-- source that stores one: an email's sender and recipient names, a chat
-- sender's push name and a Telegram peer's name. owner is the source identity
-- the name was given to, prefixed by source; contact_id is the live contact
-- the message attributes that owner to, NULL when it attributes none.
--
-- A message's sender: an inbound message's peer, else the user.
SELECT (cm.source || ':' || lower(COALESCE(NULLIF(cm.source_metadata->>'from', ''), NULLIF(cm.peer_normalized, ''), cm.peer_handle, '')))::text AS owner,
       lc.id AS contact_id,
       sender.name::text AS name
FROM comms_message cm
CROSS JOIN LATERAL (VALUES (cm.source_metadata->>'from_name'), (cm.source_metadata->>'push_name')) AS sender(name)
LEFT JOIN live_contact lc ON lc.id = cm.matched_contact_id AND cm.direction = 'inbound'
WHERE cm.deleted_at IS NULL
  AND btrim(COALESCE(sender.name, '')) <> ''
UNION
-- An email's recipients, each name paired with its address. The user's own
-- message is attributed to the recipient its copy is stored for.
SELECT (cm.source || ':' || lower(addresses.address))::text,
       lc.id,
       names.name::text
FROM comms_message cm
CROSS JOIN (VALUES ('to', 'to_names'), ('cc', 'cc_names'), ('bcc', 'bcc_names')) AS field(addresses, names)
CROSS JOIN LATERAL jsonb_array_elements_text(CASE
    WHEN jsonb_typeof(cm.source_metadata->field.addresses) = 'array'
    THEN cm.source_metadata->field.addresses ELSE '[]'::jsonb END
) WITH ORDINALITY AS addresses(address, n)
JOIN LATERAL jsonb_array_elements_text(CASE
    WHEN jsonb_typeof(cm.source_metadata->field.names) = 'array'
    THEN cm.source_metadata->field.names ELSE '[]'::jsonb END
) WITH ORDINALITY AS names(name, n) ON names.n = addresses.n
LEFT JOIN live_contact lc
  ON lc.id = cm.matched_contact_id
 AND cm.direction = 'outbound'
 AND lower(addresses.address) = lower(cm.peer_handle)
WHERE cm.deleted_at IS NULL
  AND btrim(names.name) <> ''
UNION
-- A Telegram chat's peer: its other side in a private chat, the sender of a
-- received group message.
SELECT ('telegram:' || COALESCE(tm.peer_user_id::text, NULLIF(tm.peer_username, ''), tm.peer_phone, ''))::text,
       lc.id,
       concat_ws(' ', NULLIF(tm.peer_first_name, ''), NULLIF(tm.peer_last_name, ''))::text
FROM telegram_message tm
LEFT JOIN live_contact lc ON lc.id = tm.matched_contact_id
WHERE tm.deleted_at IS NULL
  AND concat_ws(' ', NULLIF(tm.peer_first_name, ''), NULLIF(tm.peer_last_name, '')) <> ''
ORDER BY 1, 3, 2;
