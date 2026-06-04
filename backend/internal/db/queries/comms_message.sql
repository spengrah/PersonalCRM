-- name: UpsertCommsMessage :one
-- Insert-or-merge by the partial unique (source, external_id, matched_contact_id)
-- WHERE deleted_at IS NULL. Content fields are IMMUTABLE on conflict (first
-- writer wins). Provenance is merged by SET-UNION on BOTH the insert and the
-- conflict paths, so the observing account is recorded regardless of what
-- @source_metadata the caller passes (even '{}' or NULL):
--   - add @account_id to source_metadata.observed_accounts[] only if absent
--   - record this account's per-mailbox gmail id under
--     source_metadata.account_gmail_ids.<account_id> (or '__unknown__' when the
--     account is NULL — the nomsgid/missing-account edge)
-- The same three-level jsonb_set expression is applied to the caller's metadata
-- on insert and to the stored metadata on conflict; non-provenance keys the
-- caller supplies (html body, attachments[], labels, to/cc/bcc) are preserved.
-- The merge is idempotent: a same-account cursor-overlap replay re-runs this
-- upsert and neither grows observed_accounts[] (already present) nor changes
-- the gmail-id (stable per account). The ON CONFLICT clause MUST name the
-- partial index's WHERE predicate so Postgres infers the partial unique index.
INSERT INTO comms_message (
    source, external_id, thread_id, subject, body, snippet,
    peer_handle, peer_normalized, direction, sent_at, account_id,
    source_metadata, matched_contact_id
) VALUES (
    @source, @external_id, @thread_id, @subject, @body, @snippet,
    @peer_handle, @peer_normalized, @direction, @sent_at, @account_id,
    -- LEVEL 3 (outermost): set the per-account gmail id under
    -- account_gmail_ids.<account_id>. Lands only because LEVEL 2 seeded the
    -- parent object first (jsonb_set does NOT create intermediate parents).
    jsonb_set(
        -- LEVEL 2: SEED the account_gmail_ids parent to its existing value (or
        -- '{}'). REQUIRED — without it, level 3's nested-path set is a silent
        -- no-op and the gmail-id map is dropped.
        jsonb_set(
            -- LEVEL 1 (innermost): union observed_accounts[] into the caller's
            -- metadata (add @account_id only if absent).
            jsonb_set(
                @source_metadata::jsonb,
                '{observed_accounts}',
                CASE
                    WHEN @account_id::text IS NULL THEN
                        COALESCE(@source_metadata::jsonb->'observed_accounts', '[]'::jsonb)
                    WHEN COALESCE(@source_metadata::jsonb->'observed_accounts', '[]'::jsonb)
                         @> to_jsonb(ARRAY[@account_id::text]) THEN
                        COALESCE(@source_metadata::jsonb->'observed_accounts', '[]'::jsonb)
                    ELSE
                        COALESCE(@source_metadata::jsonb->'observed_accounts', '[]'::jsonb)
                            || to_jsonb(ARRAY[@account_id::text])
                END,
                TRUE
            ),
            '{account_gmail_ids}',
            COALESCE(@source_metadata::jsonb->'account_gmail_ids', '{}'::jsonb),
            TRUE
        ),
        ARRAY['account_gmail_ids', COALESCE(@account_id::text, '__unknown__')],
        to_jsonb(@gmail_message_id::text),
        TRUE
    ),
    @matched_contact_id
)
ON CONFLICT (source, external_id, matched_contact_id) WHERE deleted_at IS NULL
DO UPDATE SET
    -- Identical three-level merge, applied to the STORED metadata so the
    -- conflicting account's provenance is unioned in without disturbing the
    -- first writer's content keys.
    source_metadata = jsonb_set(
        jsonb_set(
            jsonb_set(
                comms_message.source_metadata,
                '{observed_accounts}',
                CASE
                    WHEN @account_id::text IS NULL THEN
                        COALESCE(comms_message.source_metadata->'observed_accounts', '[]'::jsonb)
                    WHEN COALESCE(comms_message.source_metadata->'observed_accounts', '[]'::jsonb)
                         @> to_jsonb(ARRAY[@account_id::text]) THEN
                        COALESCE(comms_message.source_metadata->'observed_accounts', '[]'::jsonb)
                    ELSE
                        COALESCE(comms_message.source_metadata->'observed_accounts', '[]'::jsonb)
                            || to_jsonb(ARRAY[@account_id::text])
                END,
                TRUE
            ),
            '{account_gmail_ids}',
            COALESCE(comms_message.source_metadata->'account_gmail_ids', '{}'::jsonb),
            TRUE
        ),
        ARRAY['account_gmail_ids', COALESCE(@account_id::text, '__unknown__')],
        to_jsonb(@gmail_message_id::text),
        TRUE
    )
WHERE comms_message.deleted_at IS NULL
RETURNING *;

-- name: GetCommsMessage :one
-- Natural-key lookup used by the email interaction consumer to locate the
-- content row for a (source, external_id, contact) tuple. deleted_at filtered.
SELECT * FROM comms_message
WHERE source = @source
  AND external_id = @external_id
  AND matched_contact_id = @matched_contact_id
  AND deleted_at IS NULL;

-- name: GetCommsMessageByID :one
SELECT * FROM comms_message
WHERE id = @id
  AND deleted_at IS NULL;

-- name: ListCommsMessagesByContact :many
-- Per-contact content, newest first (backs idx_comms_message_contact_sent).
SELECT * FROM comms_message
WHERE matched_contact_id = @matched_contact_id
  AND deleted_at IS NULL
ORDER BY sent_at DESC, id;

-- name: MarkCommsMessagesProcessed :execrows
-- Link content rows to the aggregated interaction + set processed_at, AND
-- clear claim columns. Two callers:
--   - email path (CommsStagingProcessor, tx-bound, via the email consumer):
--     email never claims rows, so the claim-clear is a provable NULL → NULL
--     no-op for email rows. There is NO session-scope predicate here (unlike
--     messages_message): the @session_ref arg is part of the shared
--     StagingProcessor signature but intentionally unused in the email path.
--   - chat non-tx engine path (commsMessageStoreAdapter.MarkProcessed, used by
--     the engine's extend/promote/bridge paths): those paths do not claim rows
--     or publish events, so the claim-clear keeps any leftover claim from a
--     prior pass from re-blocking the row. Mirrors MarkMessagesMessagesProcessed.
-- Idempotent: a replay finds rows already processed (processed_at IS NOT NULL)
-- and affects 0 rows, which the consumer treats as the expected replay case.
UPDATE comms_message
SET processed_at = NOW(),
    interaction_id = @interaction_id,
    claimed_at = NULL,
    claimed_session_ref = NULL
WHERE id = ANY(@message_ids::uuid[])
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: HardDeleteCommsMessagesByContact :exec
-- Test-only cleanup helper (scoped by matched_contact_id). Hard delete because
-- the upsert does not clear deleted_at on conflict, so soft-deleted rows would
-- resurrect across shared-DB test runs (gotcha table). Production code MUST NOT
-- call this.
DELETE FROM comms_message
WHERE matched_contact_id = @matched_contact_id;

-- name: ListCommsMessagesMissingParticipantNames :many
-- Keyset-paged rows for the one-time historical display-name re-derivation
-- (crm-admin --rederive-correspondence-names). Returns email rows at/after
-- @since that lack the from_name key (i.e. ingested before display-name
-- capture shipped), paged by id > @after_id so the runner advances the cursor
-- regardless of per-row outcome — a skipped/failed row never blocks later rows
-- (livelock avoidance). account_id + source_metadata.account_gmail_ids together
-- locate the per-mailbox gmail id to re-fetch. The INNER JOIN on a live contact
-- drops rows whose matched contact was soft-deleted (soft-delete does not cascade
-- to comms_message), so the re-derivation never spends Gmail quota re-fetching
-- mail for a deleted contact.
SELECT cm.id, cm.account_id, cm.source_metadata
FROM comms_message cm
JOIN contact c ON c.id = cm.matched_contact_id AND c.deleted_at IS NULL
WHERE cm.source = 'email'
  AND cm.deleted_at IS NULL
  AND cm.sent_at >= @since
  AND cm.id > @after_id
  AND NOT (cm.source_metadata ? 'from_name')
ORDER BY cm.id
LIMIT @batch_size;

-- name: BackfillCommsMessageParticipantNames :execrows
-- Additively merge the four display-name keys onto an EXISTING row's stored
-- source_metadata, preserving every existing content key (from/to/cc/bcc/
-- subject/html/attachments/labels) and the provenance keys (observed_accounts/
-- account_gmail_ids). A nested jsonb_set chain (create_missing=true per key) —
-- NOT a wholesale replace, which would destroy provenance + content. The caller
-- passes non-NULL JSON arrays ([] when empty) for *_names so jsonb_set never
-- writes a JSON null. Guarded by NOT (? 'from_name') so a row already
-- re-derived (or concurrently re-ingested with names) is a no-op (0 rows) —
-- idempotent across runs.
UPDATE comms_message
SET source_metadata = jsonb_set(
    jsonb_set(
        jsonb_set(
            jsonb_set(
                source_metadata,
                '{from_name}',
                to_jsonb(@from_name::text),
                TRUE
            ),
            '{to_names}',
            @to_names::jsonb,
            TRUE
        ),
        '{cc_names}',
        @cc_names::jsonb,
        TRUE
    ),
    '{bcc_names}',
    @bcc_names::jsonb,
    TRUE
)
WHERE id = @id
  AND deleted_at IS NULL
  AND NOT (source_metadata ? 'from_name');

-- =====================================================================
-- Source-parameterized aggregation-engine queries (GChat is the first
-- chat source on comms_message; Telegram/Messages migrate onto these
-- later). Every query takes @source so the shared comms_message table
-- isolates rows by source. Predicate shapes mirror messages_message.sql
-- verbatim; the only structural difference is the @source filter and the
-- thread_id column (vs. messages' chat_guid). All scope deleted_at IS NULL.
-- =====================================================================

-- name: ListUnprocessedCommsContactIDs :many
-- Claim-aware filter — same predicate shape as ListUnprocessedMessagesContactIDs,
-- plus the @source scope. matched_contact_id is NOT NULL on comms_message;
-- the IS NOT NULL guard is retained for parity/safety (harmless).
SELECT DISTINCT matched_contact_id
FROM comms_message
WHERE source = @source
  AND matched_contact_id IS NOT NULL
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL;

-- name: ListUnprocessedCommsByContact :many
-- Eligible rows for a contact within one source. Orders by thread_id then
-- sent_at — thread_id carries the chat scope, mirroring messages' ORDER BY
-- chat_guid, sent_at.
SELECT * FROM comms_message
WHERE source = @source
  AND matched_contact_id = @matched_contact_id
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
ORDER BY thread_id, sent_at;

-- name: ListUnprocessedCommsByContactAndChat :many
-- Eligible rows for a (contact, thread/chat) pair within one source.
-- @thread_id is the chat scope (the GChat space resource name).
SELECT * FROM comms_message
WHERE source = @source
  AND matched_contact_id = @matched_contact_id
  AND thread_id = @thread_id
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
ORDER BY sent_at;

-- name: ListUnprocessedCommsChatsByContact :many
-- Distinct thread_id (chat scope) values for a contact with at least one
-- eligible row within one source. Backs the PerSourceChatListerRegistry entry
-- (the worker's per-chat loop), mirroring ListUnprocessedMessagesChatsByContact.
-- NOT a MessageStore interface method. thread_id is nullable on comms_message;
-- the Go wrapper filters NULLs defensively (chat sources always write it
-- non-null).
SELECT DISTINCT thread_id
FROM comms_message
WHERE source = @source
  AND matched_contact_id = @matched_contact_id
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
ORDER BY thread_id ASC;

-- name: GetCommsMessageByReplyTarget :one
-- Resolves the row a reply points at. comms_message has NO reply_to column;
-- the reply target is itself a stored message, looked up by its own external_id
-- within the same (source, thread/chat) scope. Used by the aggregator's
-- explicit-reply-bridge path. Intentionally does NOT filter processed_at — a
-- reply can target an already-processed message (the whole point of bridging).
SELECT * FROM comms_message
WHERE source = @source
  AND thread_id = @thread_id
  AND external_id = @reply_target_id
  AND deleted_at IS NULL;

-- name: MarkCommsMessagesProcessedForSession :execrows
-- Tx-bound, session-scoped variant for the chat create-path. Mirror of
-- MarkMessagesMessagesProcessedForSession — used by the InteractionRecorder
-- consumer (via CommsSessionStagingProcessor) when processing a create-path
-- event. The (claimed_session_ref = @session_ref OR IS NULL) predicate is the
-- boundary-shift defense the recorder's zero-rows-affected rollback depends on:
-- it rejects rows claimed for a DIFFERENT session while accepting rows the
-- non-tx publish path left unclaimed. This is SEPARATE from the email
-- MarkCommsMessagesProcessed (which has no session predicate — email never
-- claims, so scoping by session would wrongly reject email rows).
--
-- Returns rows affected. The consumer distinguishes three cases:
--   - affected == len(message_ids): happy path.
--   - affected == 0 on a fresh write: predicate filtered everything out
--     (boundary-shift race). Caller MUST roll back the tx.
--   - affected == 0 on a replay: expected; rows already linked on the
--     original attempt.
UPDATE comms_message
SET processed_at = NOW(),
    interaction_id = @interaction_id,
    claimed_at = NULL,
    claimed_session_ref = NULL
WHERE id = ANY(@message_ids::uuid[])
  AND (claimed_session_ref = @session_ref OR claimed_session_ref IS NULL)
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: ClaimCommsMessages :many
-- Race-safe conditional claim — mirror of ClaimMessagesMessages. No @source
-- filter needed: id is the PK and globally unique; the caller already scoped to
-- source when it listed the rows. Returns the IDs actually claimed so the engine
-- can detect partial claims.
UPDATE comms_message
SET claimed_at = NOW(),
    claimed_session_ref = @session_ref
WHERE id = ANY(@message_ids::uuid[])
  AND processed_at IS NULL
  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')
  AND deleted_at IS NULL
RETURNING id;

-- name: ClearStaleCommsClaim :exec
-- Defensive recovery branch: clears claim columns for rows still carrying the
-- expected stale session_ref. Mirror of ClearMessagesMessageStaleClaim. Used
-- when the engine detected a recovery pass but FindEventBySource returned no
-- row.
UPDATE comms_message
SET claimed_at = NULL,
    claimed_session_ref = NULL
WHERE id = ANY(@message_ids::uuid[])
  AND claimed_session_ref = @expected_session_ref
  AND processed_at IS NULL
  AND deleted_at IS NULL;

-- name: BackdateCommsMessageClaim :exec
-- Test-only helper: ages the claim past the 5-minute TTL so a fresh
-- aggregate pass can re-claim a stale claim. Mirror of
-- BackdateMessagesMessageClaim. Production code MUST NOT call this.
UPDATE comms_message
SET claimed_at = NOW() - INTERVAL '10 minutes'
WHERE id = ANY(@message_ids::uuid[]);
