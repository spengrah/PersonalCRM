-- External Contact queries

-- name: GetExternalContact :one
SELECT * FROM external_contact WHERE id = $1;

-- name: GetExternalContactBySource :one
SELECT * FROM external_contact
WHERE source = $1 AND source_id = $2 AND COALESCE(account_id, '') = COALESCE($3, '');

-- name: UpsertExternalContact :one
INSERT INTO external_contact (
    source, source_id, account_id, display_name, first_name, last_name,
    emails, phones, addresses, organization, job_title, birthday, photo_url,
    crm_contact_id, match_status, duplicate_of_id, etag, metadata, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (source, source_id, COALESCE(account_id, '')) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    first_name = EXCLUDED.first_name,
    last_name = EXCLUDED.last_name,
    emails = EXCLUDED.emails,
    phones = EXCLUDED.phones,
    addresses = EXCLUDED.addresses,
    organization = EXCLUDED.organization,
    job_title = EXCLUDED.job_title,
    birthday = EXCLUDED.birthday,
    photo_url = EXCLUDED.photo_url,
    etag = EXCLUDED.etag,
    metadata = EXCLUDED.metadata,
    synced_at = EXCLUDED.synced_at,
    updated_at = NOW()
RETURNING *;

-- name: ListExternalContactsBySource :many
SELECT * FROM external_contact
WHERE source = $1 AND ($2::text IS NULL OR account_id = $2)
ORDER BY display_name
LIMIT $3 OFFSET $4;

-- name: ListUnmatchedExternalContacts :many
SELECT * FROM external_contact
WHERE source = $1
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL
ORDER BY display_name
LIMIT $2 OFFSET $3;

-- name: ListAllUnmatchedExternalContacts :many
SELECT * FROM external_contact
WHERE match_status = 'unmatched'
  AND duplicate_of_id IS NULL
ORDER BY source, display_name
LIMIT $1 OFFSET $2;

-- name: CountUnmatchedExternalContacts :one
SELECT COUNT(*) FROM external_contact
WHERE source = $1
  AND match_status = 'unmatched'
  AND duplicate_of_id IS NULL;

-- name: CountAllUnmatchedExternalContacts :one
SELECT COUNT(*) FROM external_contact
WHERE match_status = 'unmatched'
  AND duplicate_of_id IS NULL;

-- name: UpdateExternalContactMatch :one
UPDATE external_contact SET
    crm_contact_id = $2,
    match_status = $3,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateExternalContactDuplicate :exec
UPDATE external_contact SET
    duplicate_of_id = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: IgnoreExternalContact :exec
UPDATE external_contact SET
    match_status = 'ignored',
    updated_at = NOW()
WHERE id = $1;

-- name: FindExternalContactsByEmail :many
SELECT * FROM external_contact
WHERE emails @> $1::jsonb
  AND duplicate_of_id IS NULL
ORDER BY created_at;

-- name: FindExternalContactsByNormalizedEmail :many
SELECT * FROM external_contact
WHERE EXISTS (
    SELECT 1 FROM jsonb_array_elements(emails) AS e
    WHERE LOWER(e->>'value') = LOWER($1)
)
AND duplicate_of_id IS NULL
ORDER BY created_at;

-- name: ListExternalContactsForCRMContact :many
SELECT * FROM external_contact
WHERE crm_contact_id = $1
ORDER BY source, account_id;

-- name: DeleteExternalContactsBySourceAccount :exec
DELETE FROM external_contact
WHERE source = $1 AND COALESCE(account_id, '') = COALESCE($2, '');

-- name: DeleteExternalContact :exec
DELETE FROM external_contact WHERE id = $1;

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
