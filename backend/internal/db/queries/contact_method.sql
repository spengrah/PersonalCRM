-- Contact method queries

-- name: ListContactMethodsByContact :many
SELECT * FROM contact_method
WHERE contact_id = $1
ORDER BY
    is_primary DESC,
    CASE type
        WHEN 'email' THEN 1
        WHEN 'phone' THEN 2
        WHEN 'whatsapp' THEN 3
        WHEN 'telegram' THEN 4
        WHEN 'signal' THEN 5
        WHEN 'discord' THEN 6
        WHEN 'twitter' THEN 7
        WHEN 'gchat' THEN 8
        ELSE 99
    END,
    created_at ASC;

-- name: CreateContactMethod :one
INSERT INTO contact_method (
    contact_id,
    type,
    value,
    value_normalized,
    is_primary
) VALUES (
    $1, $2, $3, $4, $5
) RETURNING *;

-- name: DeleteContactMethodsByContact :exec
DELETE FROM contact_method
WHERE contact_id = $1;

-- name: UpdateContactMethodValue :one
UPDATE contact_method cm
SET value = $2,
    value_normalized = $3,
    updated_at = NOW()
WHERE cm.id = $1
  AND EXISTS (
    SELECT 1 FROM contact c
    WHERE c.id = cm.contact_id
      AND c.deleted_at IS NULL
  )
RETURNING *;

-- name: FindMethodsByNormalizedValue :many
SELECT cm.*, c.full_name as contact_name
FROM contact_method cm
JOIN contact c ON c.id = cm.contact_id
WHERE cm.type = ANY($1::text[])
  AND cm.value_normalized = $2
  AND c.deleted_at IS NULL;

-- name: SetContactMethodPrimary :exec
UPDATE contact_method cm
SET is_primary = $2,
    updated_at = NOW()
WHERE cm.id = $1
  AND EXISTS (
    SELECT 1 FROM contact c
    WHERE c.id = cm.contact_id
      AND c.deleted_at IS NULL
  );

-- name: ListCanonicalIdentifiersByType :many
-- Returns the deduplicated canonicalized value set for the given
-- contact_method types, scoped to non-deleted contacts. Ordered
-- alphabetically by value_normalized for deterministic daemon-side diff.
-- Used by GET /api/v1/host/:id/known-identifiers — the daemon needs the
-- SET of canonical phones/emails, not the contact mapping, so DISTINCT
-- collapses the same value across multiple contacts.
SELECT DISTINCT cm.value_normalized
FROM contact_method cm
JOIN contact c ON c.id = cm.contact_id
WHERE cm.type = ANY($1::text[])
  AND cm.value_normalized <> ''
  AND c.deleted_at IS NULL
ORDER BY cm.value_normalized ASC;

-- name: ListEmailIdentitiesForSync :many
-- Returns (value_normalized, contact_id) for every email contact_method of a
-- non-deleted contact. MANY-TO-ONE allowed: a shared address (joint inbox /
-- collision) maps to multiple contacts and each pair is returned so the Gmail
-- provider can fan out to all owners (spec §3.1). value_normalized is already
-- lowercased by the contact_method trigger. Ordered deterministically.
SELECT cm.value_normalized, cm.contact_id
FROM contact_method cm
JOIN contact c ON c.id = cm.contact_id
WHERE cm.type = 'email'
  AND cm.value_normalized <> ''
  AND c.deleted_at IS NULL
ORDER BY cm.value_normalized ASC, cm.contact_id ASC;

-- name: ListGChatIdentitiesForSync :many
-- Dual-source variant of ListEmailIdentitiesForSync for the Google Chat
-- provider. Returns (value_normalized, contact_id, source_type) for every
-- gchat OR email contact_method of a non-deleted contact. GChat sender
-- addresses ARE emails, so the provider's known-identity map must consider
-- both a dedicated 'gchat' method AND any plain 'email' method. The
-- source_type projection (cm.type, 'gchat' or 'email') is the discriminator.
-- MANY-TO-ONE allowed: a shared address maps to multiple contacts and each
-- pair is returned so the provider can fan out to all owners (spec §6).
-- value_normalized is already lowercased+trimmed by the contact_method
-- trigger for BOTH types (normalize_contact_method_value, migrations/021/022),
-- so gchat and email values normalize identically — case-insensitivity is
-- inherited, not re-implemented. Empty-normalized values are excluded.
-- The cm.type ASC tiebreaker keeps iteration / test assertions stable when
-- the same address has both a gchat and an email method on one contact.
SELECT cm.value_normalized, cm.contact_id, cm.type AS source_type
FROM contact_method cm
JOIN contact c ON c.id = cm.contact_id
WHERE cm.type IN ('gchat', 'email')
  AND cm.value_normalized <> ''
  AND c.deleted_at IS NULL
ORDER BY cm.value_normalized ASC, cm.contact_id ASC, cm.type ASC;

-- name: InsertContactMethodWithIdentity :one
-- Inserts a contact method with an EXPLICIT id and created_at so the apply
-- stage can delete a row whose (type, value_normalized) key changed and put it
-- back with its identity intact. Genuinely new rows pass generated values for
-- the same two columns, so one query serves both cases.
--
-- is_primary is always supplied as FALSE by the apply stage: promotion is the
-- last step, which is what keeps idx_contact_method_primary from being
-- violated mid-apply.
--
-- Deliberately NO "ON CONFLICT DO NOTHING": the in-memory fold has already
-- proven the row is absent, so a conflict here means the fold is wrong and
-- must fail loudly rather than be swallowed.
--
-- value_normalized is written by the set_contact_method_value_normalized
-- trigger (migration 022), which overwrites whatever is passed. The
-- placeholder below is never the stored value — do not read the returned
-- value_normalized as confirmation of what Go computed.
INSERT INTO contact_method (
    id,
    contact_id,
    type,
    value,
    value_normalized,
    is_primary,
    created_at
) VALUES (
    sqlc.arg(id), sqlc.arg(contact_id), sqlc.arg(type), sqlc.arg(value), '', sqlc.arg(is_primary), sqlc.arg(created_at)
) RETURNING *;

-- name: UpdateContactMethodByContact :one
-- In-place value/type update for a row whose (type, value_normalized) key is
-- UNCHANGED — the apply stage's step 4. A key change goes through
-- delete-and-reinsert instead; this query exists for the case where the user
-- edited the stored spelling ("Case@Example.test" -> "case@example.test", or a
-- phone respelling) without moving the normalized key, which steps 1-3 would
-- otherwise silently discard.
--
-- Scoped by contact_id as well as id so a method id from another contact can
-- never be acted on.
UPDATE contact_method
SET type = sqlc.arg(type),
    value = sqlc.arg(value)
WHERE id = sqlc.arg(id)
  AND contact_id = sqlc.arg(contact_id)
RETURNING *;

-- name: DeleteContactMethodByContact :exec
-- Contact-scoped single-row delete. Used both for removals and for the first
-- phase of the delete-and-reinsert applied to key-changing rows.
DELETE FROM contact_method
WHERE id = sqlc.arg(id)
  AND contact_id = sqlc.arg(contact_id);

-- name: DemoteContactMethodPrimaryByContact :exec
-- Clears is_primary on ONE named row of ONE contact.
--
-- Scoped by BOTH contact_id and id, deliberately. A contact-only demote would
-- clear whichever row happened to be primary, which is the global-demotion
-- behavior that lets a stale client drop a primary it never saw.
UPDATE contact_method
SET is_primary = FALSE
WHERE id = sqlc.arg(id)
  AND contact_id = sqlc.arg(contact_id);

-- name: PromoteContactMethodPrimaryByContact :exec
-- Sets is_primary on one named row of one contact. Added rather than reusing
-- SetContactMethodPrimary, which matches by method id alone and is used by
-- enrichment — changing it would move enrichment behavior.
UPDATE contact_method
SET is_primary = TRUE
WHERE id = sqlc.arg(id)
  AND contact_id = sqlc.arg(contact_id);

-- name: LookupContactMethodOwner :one
-- Returns the owning contact_id for a method id, regardless of which contact
-- owns it. This is what separates "this id belongs to another contact" (404 --
-- a method id is not a capability) from "this id does not exist at all"
-- (a removal succeeds as a no-op, so a retried removal is idempotent). Those
-- two cases are indistinguishable from one contact's pre-state alone.
SELECT contact_id FROM contact_method
WHERE id = sqlc.arg(id);

-- name: NormalizeContactMethodValueViaTrigger :one
-- TEST-ONLY. Calls the live normalize_contact_method_value function so the
-- C6 parity test can compare the Go mirror against the actual SQL that the
-- unique index is enforced over. Raw SQL is banned in Go including tests
-- (.ai/rules/core.md), so this query is the permitted access path.
--
-- Not used by production code. Do not make it one: production reads the
-- trigger's output from the stored column.
SELECT normalize_contact_method_value(sqlc.arg(method_type)::TEXT, sqlc.arg(raw_value)::TEXT) AS normalized;
