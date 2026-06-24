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

-- name: DeleteProvenanceLocator :exec
-- Re-extraction retirement (a later layer): drop a single locator. When the last
-- locator is removed the write API retracts the assertion.
DELETE FROM assertion_provenance
WHERE assertion_id = $1 AND locator_hash = $2;

-- name: ExistsCommsMessage :one
-- Write-time existence validation: confirm a content source row exists before
-- accepting a provenance locator that references it. One tiny query per content
-- table; source_id is parsed to UUID by the caller.
SELECT EXISTS(SELECT 1 FROM comms_message WHERE id = $1);

-- name: ExistsTelegramMessage :one
SELECT EXISTS(SELECT 1 FROM telegram_message WHERE id = $1);

-- name: ExistsMessagesMessage :one
SELECT EXISTS(SELECT 1 FROM messages_message WHERE id = $1);

-- name: ExistsMeetingNote :one
SELECT EXISTS(SELECT 1 FROM meeting_note WHERE id = $1);

-- name: ExistsCalendarEvent :one
SELECT EXISTS(SELECT 1 FROM calendar_event WHERE id = $1);

-- name: ExistsPhoneCall :one
SELECT EXISTS(SELECT 1 FROM phone_call WHERE id = $1);
