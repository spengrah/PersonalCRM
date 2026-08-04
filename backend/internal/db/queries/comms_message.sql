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

-- name: TestInsertCommsMessageLinked :one
-- Test-only: inserts a comms_message already linked to an interaction. Used by
-- the venue backfill test to seed an email/gchat thread container row.
-- Production code MUST NOT call this.
INSERT INTO comms_message (
    source, external_id, thread_id, direction, sent_at, matched_contact_id, interaction_id
) VALUES (
    @source, @external_id, @thread_id, @direction, @sent_at, @matched_contact_id, @interaction_id
)
RETURNING *;

-- name: GetCommsMessageContainer :one
-- Returns the venue-container key (source + thread_id) for a staging row by its
-- UUID. Used by the live interaction recorder to resolve the gchat venue (email
-- resolves its venue from the EmailInteractionConsumer's comms row directly).
-- The thread is consistent across all messages in one aggregated session, so
-- reading the first id is sufficient.
SELECT source, thread_id
FROM comms_message
WHERE id = $1 AND deleted_at IS NULL;

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
--   - chat non-tx engine path (commsadapter.StoreAdapter.MarkProcessed, used by
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

-- name: SoftDeleteDuplicateCommsMessagesForMerge :exec
-- Merge dedup step 1 (see CommsMessageRepository.RepointContactForMergeTx):
-- tombstone the merge source's live copy of any (source, external_id) the
-- target ALSO has live — the email-fanout shape, where one upstream message
-- produced a row under both contacts. Without this, the follow-up repoint
-- would collide on idx_comms_message_dedup (partial UNIQUE (source,
-- external_id, matched_contact_id) WHERE deleted_at IS NULL). Soft-delete
-- (not hard DELETE) preserves the row for audit, matching the table's
-- tombstone semantics; the surviving target row carries the same upstream
-- message. MUST run strictly BEFORE RepointCommsMessageContact.
UPDATE comms_message
SET deleted_at = NOW()
WHERE comms_message.matched_contact_id = sqlc.arg(source_contact_id)
  AND comms_message.deleted_at IS NULL
  AND EXISTS (
    SELECT 1 FROM comms_message t
    WHERE t.source = comms_message.source
      AND t.external_id = comms_message.external_id
      AND t.matched_contact_id = sqlc.arg(target_contact_id)
      AND t.deleted_at IS NULL
  );

-- name: RepointCommsMessageContact :exec
-- Merge dedup step 2: re-point ALL remaining source-matched rows (live +
-- soft-deleted — the dedup index is partial on deleted_at IS NULL, so
-- soft-deleted rows cannot collide; including them mirrors
-- TransferInteractions' includes-soft-deleted audit stance). Runs strictly
-- AFTER SoftDeleteDuplicateCommsMessagesForMerge inside
-- RepointContactForMergeTx's savepoint.
UPDATE comms_message
SET matched_contact_id = sqlc.arg(target_contact_id)
WHERE matched_contact_id = sqlc.arg(source_contact_id);

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
-- within the same (source, contact, thread/chat) scope. Used by the
-- aggregator's explicit-reply-bridge path. Scoping to matched_contact_id is
-- load-bearing: comms_message is per-contact (a shared address fans out to one
-- row per matched contact), so an unscoped lookup could return a DIFFERENT
-- contact's row, whose interaction_id points at that other contact's
-- interaction — which the bridge would then wrongly promote to mutual.
-- Intentionally does NOT filter processed_at — a reply can target an
-- already-processed message (the whole point of bridging).
SELECT * FROM comms_message
WHERE source = @source
  AND matched_contact_id = @matched_contact_id
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

-- name: SoftDeleteCommsMessageByID :exec
-- Test-only helper: soft-deletes a single comms_message row by id, simulating
-- the upstream delete a chat provider would observe. Used by the delete-no-op
-- aggregation test. There is no production chat delete path yet.
UPDATE comms_message
SET deleted_at = NOW()
WHERE id = @id
  AND deleted_at IS NULL;

-- =====================================================================
-- GChat edit/delete reconciliation queries. Scoped by (source,
-- external_id) so they hit ALL fanned-out rows for one upstream message
-- (a message qualifying N contacts has N rows sharing external_id). All
-- scope deleted_at IS NULL.
-- =====================================================================

-- name: ApplyCommsMessageEditByExternalID :execrows
-- Apply an upstream message edit to every stored row for (source, external_id).
-- Updates body + snippet, pushes the row's prior body onto
-- source_metadata.previous_bodies[] (newest-first, capped at the last 3), and
-- records the edit's last_update_time + edited_at. processed_at, interaction_id,
-- and deleted_at are deliberately left untouched: the aggregation engine skips
-- rows with processed_at set, so a content edit never reprocesses the derived
-- interaction.
--
-- RECENCY + IDEMPOTENCY GUARD: the WHERE clause compares the stored
-- last_update_time against @last_update_time as a TIMESTAMPTZ, not as a string.
-- RFC-3339 is NOT lexicographically ordered when fractional-second precision
-- varies (e.g. "...10:00:00Z" sorts after "...10:00:00.001Z" yet is earlier),
-- so a string compare would silently reject a legitimate newer edit. Casting
-- both sides to timestamptz compares real instants regardless of precision or
-- offset. NULLIF(...,'')::timestamptz handles never-edited rows (absent/empty
-- key -> NULL -> '-infinity' floor, so the first edit always passes). Because
-- only the first writer advances last_update_time, two connected accounts
-- observing the same edit in concurrent sweeps each attempt the UPDATE but only
-- one pushes previous_bodies[] — the second's predicate is false (0 rows).
--
-- previous_bodies[] cap: prepend the row's CURRENT body (the soon-to-be-prior
-- value), then keep the first 3 elements (newest-first). A NULL current body is
-- not pushed (nothing to preserve).
UPDATE comms_message
SET body = @body,
    snippet = @snippet,
    source_metadata = jsonb_set(
        jsonb_set(
            jsonb_set(
                source_metadata,
                '{previous_bodies}',
                (
                    SELECT COALESCE(jsonb_agg(prior_body), '[]'::jsonb)
                    FROM (
                        SELECT prior_body
                        FROM jsonb_array_elements(
                            CASE
                                WHEN comms_message.body IS NULL THEN
                                    COALESCE(source_metadata->'previous_bodies', '[]'::jsonb)
                                ELSE
                                    to_jsonb(ARRAY[comms_message.body])
                                        || COALESCE(source_metadata->'previous_bodies', '[]'::jsonb)
                            END
                        ) AS prior_body
                        LIMIT 3
                    ) AS capped
                ),
                TRUE
            ),
            '{edited_at}',
            to_jsonb(@edited_at::text),
            TRUE
        ),
        '{last_update_time}',
        to_jsonb(@last_update_time::text),
        TRUE
    )
WHERE source = @source
  AND external_id = @external_id
  AND deleted_at IS NULL
  AND COALESCE(
        NULLIF(source_metadata->>'last_update_time', '')::timestamptz,
        '-infinity'::timestamptz
      ) < (@last_update_time::text)::timestamptz;

-- name: SoftDeleteCommsMessagesByExternalID :execrows
-- Soft-delete every stored row for (source, external_id) — the production chat
-- delete path. Scoped by (source, external_id) so all fanned-out contacts'
-- rows drop out of future aggregation. Idempotent: an already-deleted message
-- affects 0 rows.
UPDATE comms_message
SET deleted_at = @now
WHERE source = @source
  AND external_id = @external_id
  AND deleted_at IS NULL;

-- name: GetCommsMessageLatestByExternalID :one
-- Read one stored row for (source, external_id), newest first, to supply the
-- current body for the provider's bodyDiffers no-op-avoidance pre-check. Fanned-
-- out rows share content, so any one row suffices. Used ONLY for body
-- comparison — NEVER for a Go-side timestamp/recency comparison (recency is the
-- SQL ::timestamptz guard in ApplyCommsMessageEditByExternalID).
SELECT * FROM comms_message
WHERE source = @source
  AND external_id = @external_id
  AND deleted_at IS NULL
ORDER BY sent_at DESC, id
LIMIT 1;

-- =====================================================================
-- Chat-source staging writes that allow an UNRESOLVED peer (076).
-- comms_message.matched_contact_id is nullable for source='whatsapp' only
-- (comms_message_contact_source_check). These five queries are the whole
-- surface: two upserts split by the nil-ness of the contact, the
-- reconcile+attach pair the retroactive link runs in one transaction, and
-- the discovery aggregate.
-- =====================================================================

-- name: UpsertChatCommsMessageMatched :one
-- Chat-source staging write for a message whose peer IS a known contact.
-- Content is IMMUTABLE on conflict (first writer wins), which makes the live
-- ingest path and the link-time history backfill idempotent against each
-- other. No provenance jsonb merge: chat sources observe from exactly one
-- linked device, so UpsertCommsMessage's observed_accounts/account_gmail_ids
-- machinery has nothing to merge.
-- Conflict target is idx_comms_message_dedup (058).
INSERT INTO comms_message (
    source, external_id, thread_id, body, snippet,
    peer_handle, peer_normalized, direction, sent_at, account_id,
    source_metadata, matched_contact_id
) VALUES (
    @source, @external_id, @thread_id, @body, @snippet,
    @peer_handle, @peer_normalized, @direction, @sent_at, @account_id,
    @source_metadata, @matched_contact_id
)
ON CONFLICT (source, external_id, matched_contact_id) WHERE deleted_at IS NULL
-- No-op touch so RETURNING yields the stored row; a bare DO NOTHING returns no
-- rows, which the repository would report as a spurious not-found.
DO UPDATE SET source = comms_message.source
WHERE comms_message.deleted_at IS NULL
RETURNING *;

-- name: UpsertChatCommsMessageUnmatched :one
-- Same, for a message whose peer is not (yet) a contact. matched_contact_id is
-- written NULL; the row is invisible to every eligible/aggregation query until
-- AttachUnmatchedCommsMessagesByPeer fills it in. A non-whatsapp source hits
-- comms_message_contact_source_check here, which is the point.
-- Conflict target is idx_comms_message_dedup_unmatched (076).
INSERT INTO comms_message (
    source, external_id, thread_id, body, snippet,
    peer_handle, peer_normalized, direction, sent_at, account_id,
    source_metadata
) VALUES (
    @source, @external_id, @thread_id, @body, @snippet,
    @peer_handle, @peer_normalized, @direction, @sent_at, @account_id,
    @source_metadata
)
ON CONFLICT (source, external_id) WHERE matched_contact_id IS NULL AND deleted_at IS NULL
DO UPDATE SET source = comms_message.source
WHERE comms_message.deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteDuplicateUnmatchedCommsMessages :execrows
-- Reconciliation half of the retroactive attach. An unmatched row whose
-- (source, external_id) is ALREADY staged against @contact_id is a duplicate of
-- that matched row — the chat upsert is content-immutable, so the two carry the
-- same content — and attaching it would violate idx_comms_message_dedup and
-- abort the caller's tx. Soft-delete it instead of stranding it unmatched
-- forever (the realistic sequence: a LID-only peer staged unmatched, then the
-- same message re-staged matched once the phone resolved). Runs BEFORE
-- AttachUnmatchedCommsMessagesByPeer, in the same transaction.
UPDATE comms_message
SET deleted_at = NOW()
WHERE comms_message.source = @source
  AND comms_message.matched_contact_id IS NULL
  AND comms_message.deleted_at IS NULL
  AND (
        (sqlc.narg(peer_handle)::text IS NOT NULL AND comms_message.peer_handle = sqlc.narg(peer_handle)::text)
     OR (sqlc.narg(peer_normalized)::text IS NOT NULL AND comms_message.peer_normalized = sqlc.narg(peer_normalized)::text)
  )
  AND EXISTS (
        SELECT 1 FROM comms_message t
        WHERE t.source = comms_message.source
          AND t.external_id = comms_message.external_id
          AND t.matched_contact_id = @contact_id
          AND t.deleted_at IS NULL
  );

-- name: AttachUnmatchedCommsMessagesByPeer :execrows
-- Retroactive attach: binds a source's remaining unmatched rows for one peer to
-- a contact. Matches on peer_handle (always present) OR peer_normalized
-- (present only when the peer's phone was resolvable) so a LID-only peer
-- imported by hand still picks up its history. The colliding rows were already
-- removed by SoftDeleteDuplicateUnmatchedCommsMessages, so no NOT EXISTS guard
-- is needed here — but the pair MUST run in one transaction, which is why the
-- repository exposes a single AttachUnmatchedByPeer method rather than two.
UPDATE comms_message
SET matched_contact_id = @contact_id
WHERE source = @source
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL
  AND (
        (sqlc.narg(peer_handle)::text IS NOT NULL AND peer_handle = sqlc.narg(peer_handle)::text)
     OR (sqlc.narg(peer_normalized)::text IS NOT NULL AND peer_normalized = sqlc.narg(peer_normalized)::text)
  );

-- name: ListUnmatchedCommsPeerCounts :many
-- Discovery counts over unmatched rows for one source, grouped by peer.
-- One query serves both the batch sweep and the single-peer live check
-- (@peer_handle NULL = all peers) — a separate single-peer twin would trip
-- scripts/ci/sqlc-select-list-guard.sh. Backed by
-- idx_comms_message_unmatched_peer (076).
-- The explicit casts on the aggregate columns are load-bearing: sqlc types an
-- uncast aggregate as `interface{}`. A cast makes it concrete but also
-- non-nullable, so the two genuinely-optional columns (a LID-only peer has no
-- phone; a message may carry no push name) COALESCE to the empty string and the
-- repository maps '' back to nil — neither value is ever legitimately empty.
-- last_message_at needs no COALESCE: sent_at is NOT NULL and every returned
-- group has at least one row.
SELECT peer_handle,
       COALESCE(MAX(peer_normalized), '')::text       AS peer_normalized,
       COUNT(*)                                       AS total_count,
       COUNT(*) FILTER (WHERE direction = 'inbound')  AS inbound_count,
       COUNT(*) FILTER (WHERE direction = 'outbound') AS outbound_count,
       MAX(sent_at)::timestamptz                      AS last_message_at,
       -- Newest KNOWN push name for the peer, without a correlated subquery.
       -- The FILTER skips rows that carry no push_name, so a newer anonymous
       -- row does not hide a name an older row already reported.
       COALESCE(
           (array_agg(source_metadata->>'push_name' ORDER BY sent_at DESC)
                FILTER (WHERE source_metadata->>'push_name' IS NOT NULL))[1],
           ''
       )::text                                        AS last_push_name
FROM comms_message
WHERE source = @source
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL
  AND peer_handle IS NOT NULL
  AND (sqlc.narg(peer_handle)::text IS NULL OR peer_handle = sqlc.narg(peer_handle)::text)
GROUP BY peer_handle
HAVING COUNT(*) >= @min_messages::bigint
ORDER BY total_count DESC, peer_handle ASC;

-- name: GetOldestCommsMessageSentAtForSource :one
-- Oldest staged sent_at for one source, over live rows (matched or not). Backs
-- the WhatsApp status endpoint's observed backfill floor — the empirical answer
-- to "how deep did the one-shot history actually reach". Returns NULL when the
-- source has staged nothing yet.
SELECT MIN(sent_at)::timestamptz AS oldest_sent_at
FROM comms_message
WHERE source = @source
  AND deleted_at IS NULL;

-- name: HardDeleteCommsMessagesBySourceAndExternalIDPrefix :exec
-- Test-only cleanup helper for rows that have NO contact to scope by (the
-- unmatched chat-staging rows). Hard delete, because the chat upsert does not
-- clear deleted_at on conflict, so a soft-deleted row would resurrect across
-- shared-DB runs — and because 076's down migration refuses to revert while any
-- NULL-contact row exists. Production code MUST NOT call this.
DELETE FROM comms_message
WHERE source = @source
  AND external_id LIKE @external_id_prefix;

-- name: SoftDeleteUnmatchedChatMessageTwin :execrows
-- Tombstones the unmatched staging row for a message that has just been staged
-- WITH a contact. The matched and unmatched chat upserts have DISJOINT conflict
-- targets, so both rows can exist for one message; the matched row is the
-- survivor. O(1) on idx_comms_message_dedup_unmatched.
UPDATE comms_message
SET deleted_at = NOW()
WHERE source = @source
  AND external_id = @external_id
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL;

-- name: CountLiveMatchedChatMessage :one
-- Reports whether the message is ALREADY staged against a contact, so the
-- unmatched insert path can decline rather than mint the duplicate pair with
-- nothing to tombstone it. O(1) on idx_comms_message_dedup.
SELECT COUNT(*) FROM comms_message
WHERE source = @source
  AND external_id = @external_id
  AND matched_contact_id IS NOT NULL
  AND deleted_at IS NULL;
