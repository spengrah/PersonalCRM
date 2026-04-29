-- Interaction queries

-- name: GetInteraction :one
SELECT * FROM interaction WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContactInteractions :many
SELECT * FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateInteraction :one
INSERT INTO interaction (contact_id, source, source_ref, occurred_at, description, direction)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.narg('direction'), 'mutual'))
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
