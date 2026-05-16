-- External Contact queries

-- name: GetExternalContact :one
-- Tombstoned rows are not retrievable by ID through normal flows.
SELECT * FROM external_contact
WHERE id = $1
  AND deleted_at IS NULL;

-- name: FindExternalContactsBySourceAndSourceID :many
-- Finds all unmatched external_contact rows for a (source, source_id) pair
-- regardless of account_id. Used by the calendar rematch handler to mark
-- gcal_attendee import candidates as matched after a CRM contact links them.
-- Tombstoned candidates must not be rematched.
SELECT * FROM external_contact
WHERE source = sqlc.arg('source')
  AND source_id = sqlc.arg('source_id')
  AND match_status = 'unmatched'
  AND deleted_at IS NULL;

-- name: GetExternalContactBySource :one
-- Tombstone-aware: returns tombstoned rows too. The mac-daemon ingest path
-- needs visibility into tombstoned rows so it can revive them on a fresh
-- external_contact.upserted event. Existing telegram / gcontacts callers
-- never tombstone, so the broader read does not affect them. Callers that
-- want live-only rows must check `DeletedAt == nil` after the call.
SELECT * FROM external_contact
WHERE source = $1 AND source_id = $2 AND COALESCE(account_id, '') = COALESCE($3, '');

-- name: UpsertExternalContact :one
-- Named-param variant. host_id is on INSERT only and NOT in the
-- ON CONFLICT DO UPDATE SET list — the ORIGINAL paired host's
-- ownership persists across content updates. Re-pair onto a different
-- Mac is a documented limitation; the operator script
-- scripts/admin/reset_icloud_contacts.sh handles cleanup.
--
-- last_content_hash is written on both INSERT and UPDATE so the
-- /known-ids endpoint always returns the most recent payload's hash.
-- Application-layer regex enforces lowercase 64-char hex on write;
-- this query stores the value verbatim.
INSERT INTO external_contact (
    source, source_id, account_id, host_id, display_name, first_name, last_name,
    emails, phones, addresses, organization, job_title, birthday, photo_url,
    crm_contact_id, match_status, duplicate_of_id, etag, metadata, synced_at,
    last_content_hash
) VALUES (
    sqlc.arg('source'),
    sqlc.arg('source_id'),
    sqlc.arg('account_id'),
    sqlc.arg('host_id'),
    sqlc.arg('display_name'),
    sqlc.arg('first_name'),
    sqlc.arg('last_name'),
    sqlc.arg('emails'),
    sqlc.arg('phones'),
    sqlc.arg('addresses'),
    sqlc.arg('organization'),
    sqlc.arg('job_title'),
    sqlc.arg('birthday'),
    sqlc.arg('photo_url'),
    sqlc.arg('crm_contact_id'),
    sqlc.arg('match_status'),
    sqlc.arg('duplicate_of_id'),
    sqlc.arg('etag'),
    sqlc.arg('metadata'),
    sqlc.arg('synced_at'),
    sqlc.arg('last_content_hash')
)
ON CONFLICT (source, source_id, COALESCE(account_id, '')) DO UPDATE SET
    -- host_id intentionally NOT updated — preserves first-insert ownership.
    display_name      = EXCLUDED.display_name,
    first_name        = EXCLUDED.first_name,
    last_name         = EXCLUDED.last_name,
    emails            = EXCLUDED.emails,
    phones            = EXCLUDED.phones,
    addresses         = EXCLUDED.addresses,
    organization      = EXCLUDED.organization,
    job_title         = EXCLUDED.job_title,
    birthday          = EXCLUDED.birthday,
    photo_url         = EXCLUDED.photo_url,
    etag              = EXCLUDED.etag,
    metadata          = EXCLUDED.metadata,
    synced_at         = EXCLUDED.synced_at,
    last_content_hash = EXCLUDED.last_content_hash,
    updated_at        = NOW()
RETURNING *;

-- name: ListKnownExternalContactIDsByHostAndSource :many
-- Returns (source_id, last_content_hash) for every live
-- external_contact row owned by the given (host_id, source). Powers
-- GET /api/v1/host/:id/sync/:source/known-ids. Tombstoned rows are
-- excluded — the daemon's set-diff reconciliation requires that
-- rows the Pi has soft-deleted are NOT reported as known (else the
-- daemon never re-tombstones them).
--
-- Lowercase-hex of last_content_hash is enforced upstream by the
-- ingest layer's verifyExternalContactInvariants regex
-- (^[a-f0-9]{64}$) plus the JCS hash-verification check. This query
-- stores and returns the value verbatim. Legacy rows whose
-- last_content_hash is NULL cause the daemon to fall back to the
-- @deleted@unknown sentinel per the spec.
SELECT source_id, last_content_hash
FROM external_contact
WHERE host_id = sqlc.arg('host_id')
  AND source = sqlc.arg('source')
  AND deleted_at IS NULL
ORDER BY source_id;

-- name: ReviveExternalContact :one
-- Clears deleted_at on a tombstoned row. Defensive WHERE deleted_at IS NOT
-- NULL keeps the statement idempotent across concurrent revive races.
-- Preserves crm_contact_id, match_status, and all content columns.
UPDATE external_contact SET
    deleted_at = NULL,
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NOT NULL
RETURNING *;

-- name: SoftDeleteExternalContact :exec
-- Tombstones a live row. Defensive WHERE deleted_at IS NULL keeps the
-- statement idempotent against a concurrent delete. crm_contact_id,
-- match_status, and duplicate_of_id are preserved per the external_contact
-- soft-delete contract.
UPDATE external_contact SET
    deleted_at = NOW(),
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL;

-- name: ListExternalContactsBySource :many
SELECT * FROM external_contact
WHERE source = $1
  AND ($2::text IS NULL OR account_id = $2)
  AND deleted_at IS NULL
ORDER BY display_name
LIMIT $3 OFFSET $4;

-- name: ListUnmatchedExternalContacts :many
SELECT * FROM external_contact
WHERE source = $1
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
ORDER BY display_name
LIMIT $2 OFFSET $3;

-- name: ListAllUnmatchedExternalContacts :many
SELECT * FROM external_contact
WHERE match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
ORDER BY source, display_name
LIMIT $1 OFFSET $2;

-- name: CountUnmatchedExternalContacts :one
SELECT COUNT(*) FROM external_contact
WHERE source = $1
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL;

-- name: CountAllUnmatchedExternalContacts :one
SELECT COUNT(*) FROM external_contact
WHERE match_status = 'unmatched'
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL;

-- name: UpdateExternalContactMatch :one
-- Filter `deleted_at IS NULL` so a tombstoned row cannot have its
-- match state mutated. A tombstoned row is invisible to every read
-- path; writes should be invisible too.
UPDATE external_contact SET
    crm_contact_id = $2,
    match_status = $3,
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL
RETURNING *;

-- name: UpdateExternalContactDuplicate :exec
UPDATE external_contact SET
    duplicate_of_id = $2,
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL;

-- name: IgnoreExternalContact :exec
UPDATE external_contact SET
    match_status = 'ignored',
    updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL;

-- name: FindExternalContactsByEmail :many
SELECT * FROM external_contact
WHERE emails @> $1::jsonb
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at;

-- name: FindExternalContactsByNormalizedEmail :many
-- Finds external contacts whose JSONB emails contain the given normalized
-- email value. Backed by idx_external_contact_emails_value_lower_gin via
-- the jsonb_array_lower_values helper. Tombstoned rows are excluded so
-- duplicate detection ignores soft-deleted candidates.
SELECT * FROM external_contact
WHERE jsonb_array_lower_values(emails, 'value') && ARRAY[LOWER($1)]
  AND duplicate_of_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at;

-- name: ListExternalContactsForCRMContact :many
SELECT * FROM external_contact
WHERE crm_contact_id = $1
  AND deleted_at IS NULL
ORDER BY source, account_id;

-- name: DeleteExternalContactsBySourceAccount :exec
DELETE FROM external_contact
WHERE source = $1 AND COALESCE(account_id, '') = COALESCE($2, '');

-- name: DeleteExternalContact :exec
DELETE FROM external_contact WHERE id = $1;

-- name: UpsertTelegramDiscoveryCandidate :one
-- Telegram-specific upsert that preserves populated peer fields when a later
-- message arrives with null entity data. Never clears a name/handle that was
-- previously captured. Metadata is merged (|| operator) so keys from earlier
-- writes (e.g. username) are retained when the new map omits them.
INSERT INTO external_contact (
    source, source_id, display_name, first_name, last_name, metadata, synced_at
) VALUES ('telegram', $1, $2, $3, $4, $5, $6)
ON CONFLICT (source, source_id, COALESCE(account_id, '')) DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, external_contact.display_name),
    first_name   = COALESCE(EXCLUDED.first_name,   external_contact.first_name),
    last_name    = COALESCE(EXCLUDED.last_name,    external_contact.last_name),
    metadata     = external_contact.metadata || EXCLUDED.metadata,
    synced_at    = EXCLUDED.synced_at,
    updated_at   = NOW()
RETURNING *;

-- Contact Enrichment queries

-- name: GetEnrichmentsForContact :many
SELECT * FROM contact_enrichment
WHERE contact_id = $1
ORDER BY enriched_at DESC;

-- name: CreateEnrichment :one
INSERT INTO contact_enrichment (
    contact_id, source, account_id, field, external_contact_id, original_value
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (contact_id, source, field, COALESCE(account_id, '')) DO UPDATE SET
    external_contact_id = EXCLUDED.external_contact_id,
    original_value = EXCLUDED.original_value,
    enriched_at = NOW()
RETURNING *;

-- name: HasEnrichmentForField :one
SELECT EXISTS(
    SELECT 1 FROM contact_enrichment
    WHERE contact_id = $1 AND field = $2
);

-- name: GetEnrichmentByField :one
SELECT * FROM contact_enrichment
WHERE contact_id = $1 AND field = $2
LIMIT 1;

-- name: ListEnrichmentsBySource :many
SELECT * FROM contact_enrichment
WHERE source = $1
ORDER BY enriched_at DESC
LIMIT $2 OFFSET $3;

-- name: DeleteEnrichmentsForContact :exec
DELETE FROM contact_enrichment WHERE contact_id = $1;

-- name: DeleteEnrichment :exec
DELETE FROM contact_enrichment WHERE id = $1;
