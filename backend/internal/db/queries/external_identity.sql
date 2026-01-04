-- External identity queries for cross-platform contact identity matching

-- name: GetIdentityByID :one
SELECT * FROM external_identity
WHERE id = $1;

-- name: GetIdentityByIdentifier :one
SELECT * FROM external_identity
WHERE identifier_type = $1 AND identifier = $2 AND source = $3;

-- name: FindIdentitiesByIdentifier :many
SELECT * FROM external_identity
WHERE identifier_type = $1 AND identifier = $2;

-- name: UpsertIdentity :one
INSERT INTO external_identity (
    identifier, identifier_type, raw_identifier, source, source_id,
    contact_id, match_type, match_confidence, display_name, last_seen_at, message_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (identifier, identifier_type, source) DO UPDATE SET
    raw_identifier = COALESCE(EXCLUDED.raw_identifier, external_identity.raw_identifier),
    source_id = COALESCE(EXCLUDED.source_id, external_identity.source_id),
    contact_id = COALESCE(EXCLUDED.contact_id, external_identity.contact_id),
    match_type = COALESCE(EXCLUDED.match_type, external_identity.match_type),
    match_confidence = COALESCE(EXCLUDED.match_confidence, external_identity.match_confidence),
    display_name = COALESCE(EXCLUDED.display_name, external_identity.display_name),
    last_seen_at = EXCLUDED.last_seen_at,
    message_count = external_identity.message_count + COALESCE(EXCLUDED.message_count, 0),
    updated_at = NOW()
RETURNING *;

-- name: LinkIdentityToContact :one
UPDATE external_identity SET
    contact_id = $2,
    match_type = $3,
    match_confidence = $4,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UnlinkIdentityFromContact :one
UPDATE external_identity SET
    contact_id = NULL,
    match_type = 'unmatched',
    match_confidence = NULL,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: ListUnmatchedIdentities :many
SELECT * FROM external_identity
WHERE contact_id IS NULL
ORDER BY message_count DESC, last_seen_at DESC NULLS LAST
LIMIT $1 OFFSET $2;

-- name: CountUnmatchedIdentities :one
SELECT COUNT(*) FROM external_identity WHERE contact_id IS NULL;

-- name: ListIdentitiesForContact :many
SELECT * FROM external_identity
WHERE contact_id = $1
ORDER BY source, identifier_type;

-- name: DeleteIdentity :exec
DELETE FROM external_identity WHERE id = $1;

-- name: DeleteIdentitiesForContact :exec
DELETE FROM external_identity WHERE contact_id = $1;

-- name: UpdateIdentityMessageCount :one
UPDATE external_identity SET
    message_count = message_count + $2,
    last_seen_at = $3,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: BulkLinkIdentitiesToContact :exec
UPDATE external_identity SET
    contact_id = $2,
    match_type = $3,
    match_confidence = $4,
    updated_at = NOW()
WHERE id = ANY($1::uuid[]);

-- name: ListIdentitiesBySource :many
SELECT * FROM external_identity
WHERE source = $1
ORDER BY last_seen_at DESC NULLS LAST
LIMIT $2 OFFSET $3;

-- name: CountIdentitiesBySource :one
SELECT COUNT(*) FROM external_identity WHERE source = $1;
