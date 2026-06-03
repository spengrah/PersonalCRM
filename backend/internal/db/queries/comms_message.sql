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
-- Link content rows to the aggregated interaction + set processed_at. Email
-- aggregation uses a deterministic source_ref and never claims rows, so there
-- is NO session-scope predicate here (unlike messages_message): the @session_ref
-- arg is part of the shared StagingProcessor signature but intentionally unused
-- in the email predicate. Idempotent: a replay finds rows already processed
-- (processed_at IS NOT NULL) and affects 0 rows, which the consumer treats as
-- the expected replay case.
UPDATE comms_message
SET processed_at = NOW(),
    interaction_id = @interaction_id
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

-- name: ListCommsMessageParticipantsSince :many
-- Stream recent email content rows for the correspondence-enrichment scan:
-- source_metadata carries the from/to/cc/bcc participant lists (bare addresses)
-- plus the index-aligned from_name/to_names/cc_names/bcc_names. matched_contact_id
-- is the known contact the message qualified for (the co-occurring contact the
-- producer records as evidence). Bounded by @since to keep each scan cheap; the
-- scan is idempotent so re-running the same window is harmless. The INNER JOIN on
-- a live contact drops rows whose matched contact was soft-deleted: a contact's
-- soft-delete (UPDATE deleted_at) does NOT cascade to its comms_message rows (the
-- FK cascade only fires on a hard DELETE), so without this join the producer would
-- keep mining correspondence with a deleted contact.
SELECT cm.matched_contact_id, cm.sent_at, cm.source_metadata
FROM comms_message cm
JOIN contact c ON c.id = cm.matched_contact_id AND c.deleted_at IS NULL
WHERE cm.source = 'email'
  AND cm.deleted_at IS NULL
  AND cm.sent_at >= @since
ORDER BY cm.sent_at DESC, cm.id;

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
