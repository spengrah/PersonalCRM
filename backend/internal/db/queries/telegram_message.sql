-- name: UpsertTelegramMessage :one
INSERT INTO telegram_message (
    telegram_message_id, telegram_chat_id, chat_type, chat_title,
    message_text, message_type, sent_at, edited_at, is_outgoing,
    reply_to_msg_id, peer_user_id, peer_username, peer_first_name,
    peer_last_name, peer_phone, peer_entity_resolved
) VALUES (
    @telegram_message_id, @telegram_chat_id, @chat_type, @chat_title,
    @message_text, @message_type, @sent_at, @edited_at, @is_outgoing,
    @reply_to_msg_id, @peer_user_id, @peer_username, @peer_first_name,
    @peer_last_name, @peer_phone, @peer_entity_resolved
)
ON CONFLICT (telegram_chat_id, telegram_message_id) DO UPDATE SET
    message_text = EXCLUDED.message_text,
    edited_at = COALESCE(EXCLUDED.edited_at, telegram_message.edited_at),
    peer_username = COALESCE(EXCLUDED.peer_username, telegram_message.peer_username),
    peer_first_name = COALESCE(EXCLUDED.peer_first_name, telegram_message.peer_first_name),
    peer_last_name = COALESCE(EXCLUDED.peer_last_name, telegram_message.peer_last_name),
    peer_phone = COALESCE(EXCLUDED.peer_phone, telegram_message.peer_phone),
    -- An authoritative update (resolved=true) must "stick" — never let a
    -- subsequent sparse re-ingest of the same message id downgrade the row.
    peer_entity_resolved = telegram_message.peer_entity_resolved OR EXCLUDED.peer_entity_resolved
RETURNING *;

-- name: SoftDeleteTelegramMessages :exec
UPDATE telegram_message
SET deleted_at = NOW()
WHERE telegram_message_id = ANY(@message_ids::int[])
  AND deleted_at IS NULL;

-- name: SoftDeleteTelegramChannelMessages :exec
UPDATE telegram_message
SET deleted_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id
  AND telegram_message_id = ANY(@message_ids::int[])
  AND deleted_at IS NULL;

-- name: GetTelegramMessage :one
SELECT * FROM telegram_message
WHERE telegram_chat_id = @telegram_chat_id
  AND telegram_message_id = @telegram_message_id
  AND deleted_at IS NULL;

-- name: ListTelegramMessagesByChatUnprocessed :many
SELECT * FROM telegram_message
WHERE telegram_chat_id = @telegram_chat_id
  AND processed_at IS NULL
  AND deleted_at IS NULL
ORDER BY sent_at;

-- name: CountTelegramMessagesByChat :many
SELECT telegram_chat_id, COUNT(*) as message_count
FROM telegram_message
WHERE deleted_at IS NULL
GROUP BY telegram_chat_id;

-- name: ListUnprocessedTelegramMessagesByContactAndChat :many
SELECT * FROM telegram_message
WHERE matched_contact_id = @matched_contact_id
  AND telegram_chat_id = @telegram_chat_id
  AND processed_at IS NULL
  AND deleted_at IS NULL
ORDER BY sent_at;

-- name: ListUnprocessedTelegramMessagesByContact :many
SELECT * FROM telegram_message
WHERE matched_contact_id = @matched_contact_id
  AND processed_at IS NULL
  AND deleted_at IS NULL
ORDER BY telegram_chat_id, sent_at;

-- name: ListDistinctUnmatchedPeers :many
-- Prefer rows with username/phone for identity matching, then rows with
-- populated names so a single aggregation pass picks the most-populated row
-- per peer (avoids depending on multi-pass COALESCE accumulation in Go).
-- Treats blank strings as absent — Telegram sometimes persists '' instead of
-- NULL for entity fields on outbound private chats, and those rows must not
-- outrank a row with a real name.
SELECT DISTINCT ON (peer_user_id)
    peer_user_id, peer_username, peer_first_name, peer_last_name, peer_phone
FROM telegram_message
WHERE matched_contact_id IS NULL
  AND peer_user_id IS NOT NULL
  AND deleted_at IS NULL
ORDER BY peer_user_id,
    CASE WHEN peer_username   IS NOT NULL AND peer_username   <> '' THEN 0 ELSE 1 END,
    CASE WHEN peer_phone      IS NOT NULL AND peer_phone      <> '' THEN 0 ELSE 1 END,
    CASE WHEN peer_first_name IS NOT NULL AND peer_first_name <> '' THEN 0 ELSE 1 END,
    CASE WHEN peer_last_name  IS NOT NULL AND peer_last_name  <> '' THEN 0 ELSE 1 END,
    sent_at DESC;

-- name: UpdateTelegramMessageContact :exec
UPDATE telegram_message
SET matched_contact_id = @matched_contact_id
WHERE peer_user_id = @peer_user_id
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL;

-- name: MarkTelegramMessagesProcessed :exec
UPDATE telegram_message
SET processed_at = NOW(),
    interaction_id = @interaction_id
WHERE id = ANY(@message_ids::uuid[])
  AND deleted_at IS NULL;

-- name: ListUnprocessedContactIDs :many
SELECT DISTINCT matched_contact_id
FROM telegram_message
WHERE matched_contact_id IS NOT NULL
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: CountTelegramMessagesByPeer :many
SELECT peer_user_id,
       COUNT(*) as total_count,
       COUNT(*) FILTER (WHERE is_outgoing) as outbound_count,
       COUNT(*) FILTER (WHERE NOT is_outgoing) as inbound_count,
       MAX(sent_at) as last_message_at
FROM telegram_message
WHERE peer_user_id IS NOT NULL
  AND deleted_at IS NULL
GROUP BY peer_user_id;

-- name: CountTelegramMessagesByPeerID :one
-- Count messages for a single peer (for incremental discovery threshold check)
SELECT COUNT(*) as total_count,
       COUNT(*) FILTER (WHERE is_outgoing) as outbound_count,
       COUNT(*) FILTER (WHERE NOT is_outgoing) as inbound_count,
       MAX(sent_at) as last_message_at
FROM telegram_message
WHERE peer_user_id = @peer_user_id
  AND deleted_at IS NULL;

-- name: FindDistinctUnmatchedPeerUserIDsByUsername :many
-- Returns distinct peer_user_ids whose unmatched messages carry the given
-- normalized telegram handle. Mirrors ListDistinctUnmatchedPeers ordering:
-- when a peer has multiple rows, prefer the one with a non-blank
-- peer_username so OnPeerLinked can create the identity. Treats blank
-- strings as absent (Telegram persists '' for outbound private chats).
SELECT DISTINCT ON (peer_user_id) peer_user_id, peer_username
FROM telegram_message
WHERE LOWER(peer_username) = LOWER(@username::text)
  AND matched_contact_id IS NULL
  AND peer_user_id IS NOT NULL
  AND deleted_at IS NULL
ORDER BY peer_user_id,
    CASE WHEN peer_username IS NOT NULL AND peer_username <> '' THEN 0 ELSE 1 END,
    sent_at DESC;

-- name: FindDistinctUnmatchedPeerUserIDsByPhone :many
-- peer_phone is stored raw from MTProto (typically digits only); contact_method
-- value_normalized is E.164 with leading '+'. Compare on digits-only.
-- Same DISTINCT ON ordering as the username variant — prefer rows with a
-- non-blank peer_username so OnPeerLinked can create the identity even when
-- the matched row is found by phone.
SELECT DISTINCT ON (peer_user_id) peer_user_id, peer_username
FROM telegram_message
WHERE regexp_replace(peer_phone, '[^0-9]', '', 'g') = regexp_replace(@phone::text, '[^0-9]', '', 'g')
  AND matched_contact_id IS NULL
  AND peer_user_id IS NOT NULL
  AND deleted_at IS NULL
ORDER BY peer_user_id,
    CASE WHEN peer_username IS NOT NULL AND peer_username <> '' THEN 0 ELSE 1 END,
    sent_at DESC;

-- name: GetPeerEntityByUserID :one
-- Returns the best-known entity data for a given peer_user_id, used by the
-- live-message handler to backfill sparse entity data from the gotd/td
-- dispatcher before upserting the new message. Does NOT filter on
-- matched_contact_id — needed for rematching previously-matched peers after
-- a contact soft-delete.
--
-- Ordering: prefer the MOST RECENT row marked peer_entity_resolved (i.e.,
-- whose entity fields came from an authoritative tg.User in the update's
-- entities). This way, an authoritative-empty event ("user removed their
-- username") is respected on subsequent sparse updates rather than being
-- undone by resurrecting an older non-blank handle. Falls back to the
-- best-non-blank ordering for legacy rows where no resolved=true history
-- exists yet.
SELECT peer_user_id, peer_username, peer_first_name, peer_last_name, peer_phone
FROM telegram_message
WHERE peer_user_id = @peer_user_id
  AND deleted_at IS NULL
ORDER BY
    -- Authoritative rows first; among them, the most recent wins.
    peer_entity_resolved DESC,
    CASE WHEN peer_entity_resolved THEN sent_at END DESC NULLS LAST,
    -- Fallback for legacy data with no resolved=true history.
    CASE WHEN peer_username   IS NOT NULL AND peer_username   <> '' THEN 0 ELSE 1 END,
    CASE WHEN peer_phone      IS NOT NULL AND peer_phone      <> '' THEN 0 ELSE 1 END,
    CASE WHEN peer_first_name IS NOT NULL AND peer_first_name <> '' THEN 0 ELSE 1 END,
    CASE WHEN peer_last_name  IS NOT NULL AND peer_last_name  <> '' THEN 0 ELSE 1 END,
    sent_at DESC
LIMIT 1;

-- name: CountUnmatchedMessagesByPeer :one
-- Counts messages about to be linked for a given peer. Read BEFORE
-- OnPeerLinked so the matched-count reporting observes the pre-link state.
SELECT COUNT(*) FROM telegram_message
WHERE peer_user_id = @peer_user_id
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL;

-- GetTelegramMessageByReplyTo removed: identical to GetTelegramMessage.
-- Use GetMessage repo method for reply resolution.

-- name: HardDeleteTelegramMessagesByChatIDRange :exec
-- Test-only: hard-deletes telegram_message rows whose telegram_chat_id
-- falls in [lo, hi]. Used by integration tests to cleanly purge per-run
-- chat ID ranges so soft-deleted rows from prior runs do not resurrect
-- on the next UpsertTelegramMessage call (UpsertTelegramMessage does
-- not clear deleted_at on conflict).
DELETE FROM telegram_message
WHERE telegram_chat_id >= sqlc.arg(lo)
  AND telegram_chat_id <= sqlc.arg(hi);
