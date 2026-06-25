-- Interaction queries

-- name: GetInteraction :one
SELECT * FROM interaction WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContactInteractions :many
SELECT * FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateInteraction :one
INSERT INTO interaction (contact_id, source, source_ref, occurred_at, description, direction, venue_id)
VALUES (
    sqlc.arg('contact_id'),
    sqlc.arg('source'),
    sqlc.narg('source_ref'),
    sqlc.arg('occurred_at'),
    sqlc.narg('description'),
    COALESCE(sqlc.narg('direction'), 'mutual'),
    sqlc.narg('venue_id')
)
RETURNING *;

-- name: UpdateInteractionDirection :one
-- Promote an outbound interaction to mutual when a reply arrives (in-place update)
UPDATE interaction
SET direction = sqlc.arg(direction),
    occurred_at = sqlc.arg(occurred_at)
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteInteraction :exec
UPDATE interaction SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountContactInteractions :one
SELECT COUNT(*) FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL;

-- name: FindInteractionBySourceRef :one
-- Find an existing interaction by contact, source, and source_ref (for deduplication)
SELECT * FROM interaction
WHERE contact_id = $1 AND source = $2 AND source_ref = $3 AND deleted_at IS NULL
LIMIT 1;

-- name: FindInteractionInWindow :one
-- Find an existing manual interaction within a time window for a given
-- direction (for manual deduplication). Direction is part of the dedup
-- key so a user logging outbound then inbound for the same contact
-- within the window correctly produces two separate rows.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND direction = sqlc.arg(direction)
  AND deleted_at IS NULL
  AND occurred_at BETWEEN sqlc.arg(window_start) AND sqlc.arg(window_end)
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentOutboundTelegramInteraction :one
-- Find the most recent outbound telegram interaction for a contact in a specific chat
-- within a time window. source_ref_prefix should include trailing % for LIKE match.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = 'telegram'
  AND direction = 'outbound'
  AND source_ref LIKE sqlc.arg(source_ref_prefix)
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentTelegramInteraction :one
-- Find the most recent telegram interaction for a contact in a specific chat
-- with a given direction. Used for incremental coalescing.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = 'telegram'
  AND direction = sqlc.arg(direction)
  AND source_ref LIKE sqlc.arg(source_ref_prefix)
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentInteractionBySourceAndDirection :one
-- Source-neutral generalization of FindRecentTelegramInteraction.
-- Used by the shared aggregator (backend/internal/messaging/aggregation)
-- for same-direction coalescing. source_ref_prefix should include
-- trailing % for LIKE match.
--
-- The ESCAPE clause lets adapters whose source-ref segments may
-- contain `_` or `%` (e.g., Apple Messages chat.guid values like
-- "iMessage;-;_chat-uuid_") supply their own escape character `\`
-- without false-matching unrelated rows. Numeric-only chat IDs
-- (Telegram) pass through unchanged since they never contain LIKE
-- wildcards.
--
-- The explicit ::text cast on the bind variable keeps sqlc inferring
-- pgtype.Text for the parameter; without it the ESCAPE clause causes
-- sqlc to fall back to []byte (Postgres treats the LIKE pattern as
-- bytea-compatible in that form). Plain LIKE-on-text behavior is what
-- the existing callers expect.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND direction = sqlc.arg(direction)
  AND source_ref LIKE sqlc.arg(source_ref_prefix)::text ESCAPE '\'
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentOutboundInteractionBySource :one
-- Source-neutral generalization of FindRecentOutboundTelegramInteraction.
-- Used by the shared aggregator for time-based reply bridging on inbound
-- sessions.
--
-- ESCAPE clause: same rationale as FindRecentInteractionBySourceAndDirection.
-- See that query for the ::text cast rationale.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND direction = 'outbound'
  AND source_ref LIKE sqlc.arg(source_ref_prefix)::text ESCAPE '\'
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: HardDeleteInteractionsBySourceRefPrefix :exec
-- Test-only: hard-deletes interactions whose source matches and source_ref
-- begins with prefix. Used by integration tests to purge per-run rows
-- cleanly; soft-delete is unsafe because the (source, source_ref) partial
-- unique constraint would block a same-source_ref re-insert on the next
-- test run even with deleted_at set.
DELETE FROM interaction
WHERE source = sqlc.arg(source)
  AND source_ref LIKE sqlc.arg(source_ref_prefix);

-- name: UpdateInteractionTimestamp :one
-- Extend an existing interaction's occurred_at and description (incremental coalescing)
UPDATE interaction
SET occurred_at = sqlc.arg(occurred_at),
    description = sqlc.arg(description)
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING *;

-- name: HasResponseAfter :one
-- Returns TRUE if any later inbound/mutual interaction exists for the
-- contact after the given outreach time. Used by the FollowUpManager's
-- out-of-order guard: an outbound event arriving after a response has
-- already landed must not produce a stale follow-up.
SELECT EXISTS (
    SELECT 1 FROM interaction
    WHERE contact_id = sqlc.arg('contact_id')
      AND direction IN ('inbound', 'mutual')
      AND occurred_at > sqlc.arg('outreach_at')
      AND deleted_at IS NULL
    LIMIT 1
) AS has_response;

-- name: ListSessionAttributedInteractions :many
-- Returns all live interactions attributed to a specific anarlog session
-- (both impromptu / orphan-with-tags entries and walk-in supplementals).
-- Used by the re-sync diff path in the meeting_note.recorded inline
-- handler to compute the (existing - desired) set that needs
-- soft-deleting.
SELECT * FROM interaction
WHERE source = 'anarlog_sessions'
  AND source_ref LIKE sqlc.arg('source_ref_prefix')
  AND deleted_at IS NULL
ORDER BY source_ref;

-- name: TestInsertInteraction :one
-- Test-only: inserts an interaction with a caller-supplied id, source, and
-- source_ref and a NULL venue_id, bypassing the recorder. Used by the venue
-- backfill migration test to stand up pre-existing (venue-less) interactions
-- that the 069 backfill then populates. Production code MUST NOT call this.
INSERT INTO interaction (id, contact_id, source, source_ref, occurred_at, direction)
VALUES (
    sqlc.arg('id'),
    sqlc.arg('contact_id'),
    sqlc.arg('source'),
    sqlc.narg('source_ref'),
    sqlc.arg('occurred_at'),
    sqlc.arg('direction')
)
RETURNING *;

-- name: AcquireSourceRefAggregateLock :exec
-- Takes a transaction-scoped advisory lock keyed on an interaction
-- aggregation source_ref. Used by the email-interaction consumer to
-- serialize all jobs for the same (contact, thread, local-day)
-- aggregation key, so the read-compute-write of the forward-only
-- occurred_at guard is atomic per key. The lock auto-releases on
-- commit/rollback. hashtextextended folds the source_ref string into the
-- bigint advisory-lock key space; a rare hash collision only
-- over-serializes two unrelated keys (a perf cost), never
-- under-serializes (a correctness cost). Mirrors the per-account
-- sync-enqueue lock in external_sync.sql.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('lock_key')::text, 0));
