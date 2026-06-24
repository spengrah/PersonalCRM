-- Assertion provenance queries (graph foundation).
--
-- Provenance carries the corroborating source locators for an assertion. The
-- write API computes locator_hash from the full locator identity; the
-- (assertion_id, locator_hash) PK makes a same-locator re-emit a no-op while a
-- genuinely different span/version inserts a new corroborating row.

-- name: InsertProvenance :execrows
-- Appends a corroborating locator. ON CONFLICT (assertion_id, locator_hash) DO
-- NOTHING makes a same-locator re-emit a no-op; :execrows returns the rows
-- affected (1 = inserted, 0 = duplicate) so the write API knows whether to emit
-- a provenance_added event.
INSERT INTO assertion_provenance (
    assertion_id, locator_hash, source_kind, source_id,
    producer_kind, producer_version, field,
    start_offset, end_offset, chunk_id, input_hash, quote
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
ON CONFLICT (assertion_id, locator_hash) DO NOTHING;

-- name: ListProvenance :many
-- All locators for an assertion, oldest first.
SELECT * FROM assertion_provenance
WHERE assertion_id = $1
ORDER BY created_at;

-- name: ListProvenanceBySource :many
-- Reverse lookup: every provenance locator a given source produced ("what did
-- this source say"), via the (source_kind, source_id) index. Backs the
-- source-row-deletion sweep (when a content row is hard-deleted, find the
-- locators that referenced it).
SELECT * FROM assertion_provenance
WHERE source_kind = $1 AND source_id = $2
ORDER BY created_at;

-- name: DeleteProvenanceLocator :exec
-- Re-extraction retirement (a later layer): drop a single locator. When the last
-- locator is removed the write API retracts the assertion.
DELETE FROM assertion_provenance
WHERE assertion_id = $1 AND locator_hash = $2;

-- name: ExistsCommsMessage :one
-- Write-time existence validation: confirm a content source row exists before
-- accepting a provenance locator that references it. One tiny query per content
-- table; source_id is parsed to UUID by the caller. The four soft-deletable
-- content tables filter deleted_at so an already-tombstoned source row does not
-- pass write-time validation (a NEW assertion may not be grounded in a dead
-- source; a source deleted AFTER the assertion degrades gracefully via the
-- preserved quote/input_hash). calendar_event/phone_call have no deleted_at.
SELECT EXISTS(SELECT 1 FROM comms_message WHERE id = $1 AND deleted_at IS NULL);

-- name: ExistsTelegramMessage :one
SELECT EXISTS(SELECT 1 FROM telegram_message WHERE id = $1 AND deleted_at IS NULL);

-- name: ExistsMessagesMessage :one
SELECT EXISTS(SELECT 1 FROM messages_message WHERE id = $1 AND deleted_at IS NULL);

-- name: ExistsMeetingNote :one
SELECT EXISTS(SELECT 1 FROM meeting_note WHERE id = $1 AND deleted_at IS NULL);

-- name: ExistsCalendarEvent :one
SELECT EXISTS(SELECT 1 FROM calendar_event WHERE id = $1);

-- name: ExistsPhoneCall :one
SELECT EXISTS(SELECT 1 FROM phone_call WHERE id = $1);
