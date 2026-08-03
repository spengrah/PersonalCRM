-- WhatsApp-owned state that is NOT message content: the durable one-shot
-- history-sync notification inbox (a media POINTER per chunk, never a body) and
-- the persistent per-chat group gate. Message content stages into
-- comms_message through the existing CommsMessageRepository and
-- queries/comms_message.sql; nothing here stores a message.

-- =====================================================================
-- History-sync notification inbox
--
-- Every transition after a claim is fenced by claim_token, so a worker whose
-- lease expired mid-work cannot clobber its successor's checkpoint or terminal
-- state. AdvancePhase is additionally fenced on the predecessor phase, so no
-- caller can skip the download or the receipt.
-- =====================================================================

-- name: RecordWhatsAppHistoryNotification :one
-- Records an outstanding chunk. Idempotent under WhatsApp's redelivery of an
-- already-recorded protocol message: the UNIQUE on protocol_msg_id turns the
-- second delivery into a no-op that still returns the original row's id.
-- The starting phase is DERIVED from the disposition rather than passed in, so
-- the "a dropped chunk has nothing to download and nothing to project" rule has
-- exactly one enforcement point: 'dropped_inline' enters at 'projected', a
-- media-backed chunk at 'recorded'.
-- @notification MUST already have InitialHistBootstrapInlinePayload nil'd.
INSERT INTO whatsapp_history_notification (
    protocol_msg_id, notification, sync_type, chunk_order, oldest_msg_ts, disposition, phase
) VALUES (
    @protocol_msg_id, @notification, @sync_type, @chunk_order, @oldest_msg_ts, @disposition,
    CASE WHEN @disposition::text = 'dropped_inline' THEN 'projected' ELSE 'recorded' END
)
ON CONFLICT (protocol_msg_id)
DO UPDATE SET protocol_msg_id = EXCLUDED.protocol_msg_id
RETURNING id;

-- name: ClaimNextWhatsAppHistoryNotification :one
-- Takes the next pending chunk, or reclaims one whose 15-minute lease expired,
-- and stamps a FRESH claim_token that every later transition must present. The
-- FOR UPDATE SKIP LOCKED subselect is the same shape comms_message's claim uses.
UPDATE whatsapp_history_notification
SET state = 'processing',
    claimed_at = NOW(),
    attempts = attempts + 1,
    claim_token = gen_random_uuid()
WHERE id = (
    SELECT id FROM whatsapp_history_notification
    WHERE state = 'pending'
       OR (state = 'processing' AND claimed_at < NOW() - INTERVAL '15 minutes')
    ORDER BY chunk_order, received_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: SaveWhatsAppHistoryCheckpoint :execrows
-- Token-fenced. Zero rows means the lease moved on and the caller must abandon
-- the chunk without writing further state.
UPDATE whatsapp_history_notification
SET checkpoint = @checkpoint
WHERE id = @id AND claim_token = @claim_token;

-- name: AdvanceWhatsAppHistoryPhase :execrows
-- Token-fenced AND predecessor-fenced. The phase = @from predicate is what
-- stops a mistaken or future caller skipping the download or the receipt: only
-- the five legal edges (recorded -> downloaded -> projected -> acked ->
-- deleted) ever change a row.
UPDATE whatsapp_history_notification
SET phase = @to_phase
WHERE id = @id AND claim_token = @claim_token AND phase = @from_phase;

-- name: MarkWhatsAppHistoryNotificationDone :execrows
-- Terminal success. Clears the lease so the row can never be reclaimed.
UPDATE whatsapp_history_notification
SET state = 'done',
    processed_at = NOW(),
    claimed_at = NULL,
    claim_token = NULL,
    last_error = NULL
WHERE id = @id AND claim_token = @claim_token;

-- name: MarkWhatsAppHistoryNotificationFailed :execrows
-- Terminal failure, reserved for errors no retry can fix at a phase where no
-- content was stored. Recoverable only by the explicit operator requeue below.
UPDATE whatsapp_history_notification
SET state = 'failed',
    last_error = @last_error,
    claimed_at = NULL,
    claim_token = NULL
WHERE id = @id AND claim_token = @claim_token;

-- name: RequeueFailedWhatsAppHistoryNotification :execrows
-- Operator path (crm-admin whatsapp requeue-history). Accepts any failed row;
-- for a dropped_inline row the retry can only re-send the receipt, since its
-- phase is already 'projected' and it has no media. Not token-fenced: a failed
-- row holds no lease.
UPDATE whatsapp_history_notification
SET state = 'pending',
    claim_token = NULL,
    claimed_at = NULL,
    last_error = NULL
WHERE id = @id AND state = 'failed';

-- name: ListWhatsAppHistoryNotifications :many
-- Status surface + crm-admin listing, in claim order.
SELECT * FROM whatsapp_history_notification
WHERE state = ANY(@states::text[])
ORDER BY chunk_order, received_at;

-- name: CountWhatsAppHistoryNotificationsByStateAndDisposition :many
-- Backs Status().Backfill, including the dropped-inline chunk count.
SELECT state, disposition, COUNT(*) AS notification_count
FROM whatsapp_history_notification
GROUP BY state, disposition
ORDER BY state, disposition;

-- =====================================================================
-- Per-chat group gate
-- =====================================================================

-- name: GetWhatsAppChatConfig :one
SELECT * FROM whatsapp_chat_config WHERE chat_jid = @chat_jid;

-- name: UpsertWhatsAppChatConfig :one
-- Mirrors UpsertTelegramChatConfig's preserve semantics: a re-observation
-- refreshes the title and member count it actually resolved (COALESCE keeps the
-- stored value when the new one is NULL) and NEVER overwrites the user's
-- status override.
INSERT INTO whatsapp_chat_config (
    chat_jid, chat_title, chat_type, member_count, status, last_lookup_at
) VALUES (
    @chat_jid, @chat_title, @chat_type, @member_count, @status, @last_lookup_at
)
ON CONFLICT (chat_jid) DO UPDATE SET
    chat_title = COALESCE(EXCLUDED.chat_title, whatsapp_chat_config.chat_title),
    chat_type = EXCLUDED.chat_type,
    member_count = COALESCE(EXCLUDED.member_count, whatsapp_chat_config.member_count),
    status = whatsapp_chat_config.status,
    last_lookup_at = COALESCE(EXCLUDED.last_lookup_at, whatsapp_chat_config.last_lookup_at),
    updated_at = NOW()
RETURNING *;

-- name: BackdateWhatsAppHistoryClaim :exec
-- Test-only helper: ages a claim past the 15-minute lease so a fresh claim can
-- reclaim it, simulating a worker that died mid-chunk. Mirror of
-- BackdateCommsMessageClaim. Production code MUST NOT call this.
UPDATE whatsapp_history_notification
SET claimed_at = NOW() - INTERVAL '30 minutes'
WHERE id = @id;
