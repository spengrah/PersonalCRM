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

-- name: ListAssertionsCitingContent :many
-- The non-correction assertions whose provenance cites a displayed content item,
-- one row per (content item, assertion), in recording order. Each displayed item
-- is named by its row (source_kinds[i], row_ids[i]) and keyed by content_refs[i].
-- A comms message is stored once per matched contact, so provenance citing any
-- copy of a displayed comms message, the same (source, external_id), cites that
-- item. A copy's liveness does not matter: contact merge and unmatched-row
-- reconciliation tombstone a duplicate copy while the surviving copy carries the
-- same message, and only a live displayed row reaches this query. The correction
-- predicates come from the catalog's correction marker (migration 089). An
-- assertion naming a soft-deleted node is not returned.
WITH displayed AS (
    SELECT
        unnest(sqlc.arg(source_kinds)::text[])::text AS source_kind,
        unnest(sqlc.arg(row_ids)::uuid[])::uuid AS row_id,
        unnest(sqlc.arg(content_refs)::text[])::text AS content_ref
), citable AS (
    SELECT d.source_kind, d.row_id, d.content_ref FROM displayed d
    UNION
    SELECT d.source_kind, sibling.id, d.content_ref
    FROM displayed d
    JOIN comms_message shown ON d.source_kind = 'comms_message' AND shown.id = d.row_id
    JOIN comms_message sibling ON sibling.source = shown.source
        AND sibling.external_id = shown.external_id
), cited AS (
    SELECT DISTINCT c.content_ref, p.assertion_id
    FROM citable c
    JOIN assertion_provenance p ON p.source_kind = c.source_kind AND p.source_id = c.row_id::text
)
SELECT
    cited.content_ref::text AS content_ref,
    a.id,
    a.predicate_key,
    subject.canonical_label AS subject_label,
    object.canonical_label AS object_label,
    a.value_text,
    a.value_num,
    a.value_date,
    a.value_bool,
    a.status
FROM cited
JOIN assertion a ON a.id = cited.assertion_id
JOIN predicate pr ON pr.key = a.predicate_key AND NOT pr.correction
JOIN node subject ON subject.id = a.subject_node_id AND subject.deleted_at IS NULL
LEFT JOIN node object ON object.id = a.object_node_id
WHERE a.object_node_id IS NULL OR object.deleted_at IS NULL
ORDER BY cited.content_ref, a.created_at, a.id;
