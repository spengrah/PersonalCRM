-- name: UpsertMessagesMessage :one
-- Insert with no-op on guid conflict. peer_normalized and matched_contact_id
-- come from the Pi ingest service identity-match path. mac_host_id is
-- provenance — multi-host dedup is by guid, not by host.
INSERT INTO messages_message (
    guid, chat_guid, peer_handle, peer_normalized,
    text, message_type, sent_at, is_outgoing, is_group_chat,
    reply_to_guid, matched_contact_id, mac_host_id
) VALUES (
    @guid, @chat_guid, @peer_handle, @peer_normalized,
    @text, @message_type, @sent_at, @is_outgoing, @is_group_chat,
    @reply_to_guid, @matched_contact_id, @mac_host_id
)
ON CONFLICT (guid) DO UPDATE SET
    -- No-op update so RETURNING reliably returns the existing row. We
    -- deliberately do NOT overwrite text or any user-visible field on
    -- conflict: a second push of the same guid is by definition the same
    -- content (chat.db rows are immutable post-iOS 16 except for edits,
    -- which are out of v1 scope per spec §2 messages → edits/deletes).
    guid = EXCLUDED.guid
RETURNING *;

-- name: GetMessagesMessage :one
-- Lookup by guid (primary external identifier). Used by reply-target
-- resolution.
SELECT * FROM messages_message
WHERE guid = @guid
  AND deleted_at IS NULL;

-- name: GetMessagesMessageByReplyTarget :one
-- Source-neutral analog of GetTelegramMessage for the aggregator's
-- explicit-reply-bridge path. chat_guid is included for scoping
-- selectivity.
SELECT * FROM messages_message
WHERE chat_guid = @chat_guid
  AND guid = @guid
  AND deleted_at IS NULL;

-- name: ListUnprocessedMessagesContactIDs :many
-- Claim-aware filter — same predicate shape as telegram_message.
SELECT DISTINCT matched_contact_id
FROM messages_message
WHERE matched_contact_id IS NOT NULL
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL;

-- name: ListUnprocessedMessagesByContact :many
SELECT * FROM messages_message
WHERE matched_contact_id = @matched_contact_id
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
ORDER BY chat_guid, sent_at;

-- name: ListUnprocessedMessagesByContactAndChat :many
SELECT * FROM messages_message
WHERE matched_contact_id = @matched_contact_id
  AND chat_guid = @chat_guid
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
ORDER BY sent_at;

-- name: MarkMessagesMessagesProcessed :exec
-- Non-tx variant. Mirror of MarkTelegramMessagesProcessed — used by the
-- engine's extend/promote/bridge paths only; no session-scope predicate
-- needed because those paths do not publish events.
UPDATE messages_message
SET processed_at = NOW(),
    interaction_id = @interaction_id,
    claimed_at = NULL,
    claimed_session_ref = NULL
WHERE id = ANY(@message_ids::uuid[])
  AND deleted_at IS NULL;

-- name: MarkMessagesMessagesProcessedForSession :exec
-- Tx-bound variant. Mirror of MarkTelegramMessagesProcessedForSession —
-- used by InteractionRecorder consumer when processing a create-path
-- event. The predicate rejects rows whose claimed_session_ref differs
-- from this consumer's session (boundary-shift defense) but ALSO
-- accepts rows that were never claimed (claimed_session_ref IS NULL):
-- the non-tx publish path leaves rows unclaimed, and there's no risk
-- of cross-event overwrite when the row has not yet been processed by
-- anyone.
UPDATE messages_message
SET processed_at = NOW(),
    interaction_id = @interaction_id,
    claimed_at = NULL,
    claimed_session_ref = NULL
WHERE id = ANY(@message_ids::uuid[])
  AND (claimed_session_ref = @session_ref OR claimed_session_ref IS NULL)
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: ClaimMessagesMessages :many
-- Race-safe conditional claim — same predicate shape as
-- ClaimTelegramMessages. Returns the IDs actually claimed so the engine
-- can detect partial claims.
UPDATE messages_message
SET claimed_at = NOW(),
    claimed_session_ref = @session_ref
WHERE id = ANY(@message_ids::uuid[])
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
RETURNING id;

-- name: ClearMessagesMessageStaleClaim :exec
-- Defensive recovery branch: clears claim columns for rows still
-- carrying the expected stale session_ref. Used when the engine
-- detected a recovery pass but FindEventBySource returned no row.
UPDATE messages_message
SET claimed_at = NULL,
    claimed_session_ref = NULL
WHERE id = ANY(@message_ids::uuid[])
  AND claimed_session_ref = @expected_session_ref
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: BackdateMessagesMessageClaim :exec
-- Test-only helper: ages the claim past the 5-minute TTL. Production
-- code MUST NOT call this.
UPDATE messages_message
SET claimed_at = NOW() - INTERVAL '10 minutes'
WHERE id = ANY(@message_ids::uuid[]);

-- name: HardDeleteMessagesMessagesByMacHost :exec
-- Test-only helper (mirrors HardDeleteTelegramMessagesByChatIDRange).
-- Cleanup needs hard-delete because the upsert does not clear deleted_at
-- on conflict, so soft-deleted rows would resurrect as phantoms on the
-- next run. Scoped by mac_host_id so tests can pass a fresh mac_host UUID
-- per run and clean only their own rows.
DELETE FROM messages_message
WHERE mac_host_id = @mac_host_id;
