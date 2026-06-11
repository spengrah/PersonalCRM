-- OAuth Credential Queries

-- name: GetOAuthCredential :one
-- Get a specific OAuth credential by provider and account ID
SELECT * FROM oauth_credential
WHERE provider = $1 AND account_id = $2;

-- name: GetOAuthCredentialByID :one
-- Get a specific OAuth credential by UUID
SELECT * FROM oauth_credential
WHERE id = $1;

-- name: ListOAuthCredentials :many
-- List all OAuth credentials for a provider
SELECT * FROM oauth_credential
WHERE provider = $1
ORDER BY created_at DESC;

-- name: ListAllOAuthCredentials :many
-- List all OAuth credentials
SELECT * FROM oauth_credential
ORDER BY provider, created_at DESC;

-- name: UpsertOAuthCredential :one
-- Insert or update an OAuth credential.
-- refresh_token_nonce is kept in sync with refresh_token_encrypted: when a new
-- refresh token is provided both columns update together. When the existing
-- ciphertext is preserved, a NULL nonce (legacy row whose refresh token was
-- sealed with the shared encryption_nonce) is captured into the dedicated column
-- before encryption_nonce rotates to the new access-token nonce — otherwise the
-- preserved refresh token would become undecryptable. With no ciphertext at all
-- the nonce is NULL.
INSERT INTO oauth_credential (
    provider,
    account_id,
    account_name,
    access_token_encrypted,
    refresh_token_encrypted,
    refresh_token_nonce,
    encryption_nonce,
    token_type,
    expires_at,
    scopes
) VALUES (
    sqlc.arg(provider),
    sqlc.arg(account_id),
    sqlc.arg(account_name),
    sqlc.arg(access_token_encrypted),
    sqlc.arg(refresh_token_encrypted),
    sqlc.arg(refresh_token_nonce),
    sqlc.arg(encryption_nonce),
    sqlc.arg(token_type),
    sqlc.arg(expires_at),
    sqlc.arg(scopes)
)
ON CONFLICT (provider, account_id) DO UPDATE SET
    account_name = EXCLUDED.account_name,
    access_token_encrypted = EXCLUDED.access_token_encrypted,
    refresh_token_encrypted = COALESCE(EXCLUDED.refresh_token_encrypted, oauth_credential.refresh_token_encrypted),
    refresh_token_nonce = CASE
        WHEN EXCLUDED.refresh_token_encrypted IS NOT NULL THEN EXCLUDED.refresh_token_nonce
        WHEN oauth_credential.refresh_token_encrypted IS NOT NULL THEN COALESCE(oauth_credential.refresh_token_nonce, oauth_credential.encryption_nonce)
        ELSE NULL
    END,
    encryption_nonce = EXCLUDED.encryption_nonce,
    token_type = EXCLUDED.token_type,
    expires_at = EXCLUDED.expires_at,
    scopes = EXCLUDED.scopes,
    updated_at = NOW()
RETURNING *;

-- name: UpdateOAuthCredentialTokens :one
-- Update only the token data (for token refresh).
-- refresh_token_nonce tracks refresh_token_encrypted: it is only overwritten when
-- a new refresh token is supplied. When the stored ciphertext is preserved, a
-- NULL nonce (legacy row whose refresh token was sealed with the shared
-- encryption_nonce) is captured into the dedicated column before encryption_nonce
-- rotates to the new access-token nonce — otherwise the preserved refresh token
-- would become undecryptable. SET right-hand sides read the pre-update row, so
-- the captured values are the old ones regardless of assignment order.
UPDATE oauth_credential SET
    access_token_encrypted = sqlc.arg(access_token_encrypted),
    refresh_token_encrypted = COALESCE(sqlc.arg(refresh_token_encrypted), refresh_token_encrypted),
    refresh_token_nonce = CASE
        WHEN sqlc.arg(refresh_token_encrypted) IS NOT NULL THEN sqlc.arg(refresh_token_nonce)
        WHEN refresh_token_encrypted IS NOT NULL THEN COALESCE(refresh_token_nonce, encryption_nonce)
        ELSE NULL
    END,
    encryption_nonce = sqlc.arg(encryption_nonce),
    expires_at = sqlc.arg(expires_at),
    updated_at = NOW()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteOAuthCredential :exec
-- Delete an OAuth credential by ID
DELETE FROM oauth_credential WHERE id = $1;

-- name: DeleteOAuthCredentialByProvider :exec
-- Delete all OAuth credentials for a provider
DELETE FROM oauth_credential WHERE provider = $1;

-- name: GetOAuthCredentialStatus :one
-- Get non-sensitive credential info for display
SELECT
    id,
    provider,
    account_id,
    account_name,
    expires_at,
    scopes,
    created_at,
    updated_at
FROM oauth_credential
WHERE id = $1;

-- name: ListOAuthCredentialStatuses :many
-- List non-sensitive credential info for all credentials of a provider
SELECT
    id,
    provider,
    account_id,
    account_name,
    expires_at,
    scopes,
    created_at,
    updated_at
FROM oauth_credential
WHERE provider = $1
ORDER BY created_at DESC;

-- name: CountOAuthCredentials :one
-- Count OAuth credentials for a provider
SELECT COUNT(*) FROM oauth_credential WHERE provider = $1;
