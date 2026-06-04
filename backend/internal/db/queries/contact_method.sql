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
