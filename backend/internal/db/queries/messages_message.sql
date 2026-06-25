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

-- name: TestInsertMessagesMessageLinked :one
-- Test-only: inserts a messages_message already linked to an interaction. Used
-- by the venue backfill test to seed an iMessage chat container row. Production
-- code MUST NOT call this.
INSERT INTO messages_message (
    guid, chat_guid, peer_handle, sent_at, is_outgoing, is_group_chat, interaction_id
) VALUES (
    @guid, @chat_guid, @peer_handle, @sent_at, @is_outgoing, @is_group_chat, @interaction_id
)
RETURNING *;

-- name: GetMessagesMessageContainer :one
-- Returns the venue-container key (chat_guid + group flag) for a staging row by
-- its UUID. Used by the live interaction recorder to resolve the messages
-- venue. The container is consistent across all messages in one aggregated
-- session, so reading the first id is sufficient.
SELECT chat_guid, is_group_chat
FROM messages_message
WHERE id = $1;

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

-- name: MarkMessagesMessagesProcessedForSession :execrows
-- Tx-bound variant. Mirror of MarkTelegramMessagesProcessedForSession —
-- used by InteractionRecorder consumer when processing a create-path
-- event. The predicate rejects rows whose claimed_session_ref differs
-- from this consumer's session (boundary-shift defense) but ALSO
-- accepts rows that were never claimed (claimed_session_ref IS NULL):
-- the non-tx publish path leaves rows unclaimed, and there's no risk
-- of cross-event overwrite when the row has not yet been processed by
-- anyone.
--
-- Returns rows affected. The consumer distinguishes three cases:
--   - affected == len(message_ids): happy path.
--   - affected == 0 on a fresh write: predicate filtered everything
--     out (boundary-shift race). Caller MUST roll back the tx.
--   - affected == 0 on a replay: expected; rows were already linked
--     to the existing interaction on the original attempt.
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

-- name: ListUnprocessedMessagesChatsByContact :many
-- Distinct chat_guid values for a contact with at least one eligible
-- (unprocessed AND unclaimed-or-stale) staging row. Used by the
-- messaging aggregator worker to drive per-chat AggregateForContact
-- invocations — the chat-aware path is what preserves the engine's
-- extend/bridge/coalesce contract (see spec §3 "Stage 2 — Aggregator").
SELECT DISTINCT chat_guid
FROM messages_message
WHERE matched_contact_id = @matched_contact_id
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
ORDER BY chat_guid ASC;

-- name: ListStrandedMessagesMessages :many
-- Lists rows with matched_contact_id IS NULL — i.e., rows that the
-- ingest service accepted into staging but couldn't match to a contact
-- at the time. The crm-admin --messages-rematch-stranded command
-- iterates this list to retroactively match rows after a contact_method
-- is added.
SELECT * FROM messages_message
WHERE matched_contact_id IS NULL
  AND processed_at IS NULL
  AND deleted_at IS NULL
ORDER BY sent_at;

-- name: UpdateMatchedContactForStrandedMessage :exec
-- Sets matched_contact_id + peer_normalized on a single stranded row.
-- Scoped to rows that are still unmatched + unprocessed so a
-- concurrent ingest path (a never-stranding daemon push for a peer
-- that just got matched) cannot be overwritten by this admin path.
UPDATE messages_message
SET matched_contact_id = @matched_contact_id,
    peer_normalized    = @peer_normalized
WHERE id = @id
  AND matched_contact_id IS NULL
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: HardDeleteMessagesMessagesByMacHost :exec
-- Test-only helper (mirrors HardDeleteTelegramMessagesByChatIDRange).
-- Cleanup needs hard-delete because the upsert does not clear deleted_at
-- on conflict, so soft-deleted rows would resurrect as phantoms on the
-- next run. Scoped by mac_host_id so tests can pass a fresh mac_host UUID
-- per run and clean only their own rows.
DELETE FROM messages_message
WHERE mac_host_id = @mac_host_id;
