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
-- Insert or update an OAuth credential
INSERT INTO oauth_credential (
    provider,
    account_id,
    account_name,
    access_token_encrypted,
    refresh_token_encrypted,
    encryption_nonce,
    token_type,
    expires_at,
    scopes
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (provider, account_id) DO UPDATE SET
    account_name = EXCLUDED.account_name,
    access_token_encrypted = EXCLUDED.access_token_encrypted,
    refresh_token_encrypted = COALESCE(EXCLUDED.refresh_token_encrypted, oauth_credential.refresh_token_encrypted),
    encryption_nonce = EXCLUDED.encryption_nonce,
    token_type = EXCLUDED.token_type,
    expires_at = EXCLUDED.expires_at,
    scopes = EXCLUDED.scopes,
    updated_at = NOW()
RETURNING *;

-- name: UpdateOAuthCredentialTokens :one
-- Update only the token data (for token refresh)
UPDATE oauth_credential SET
    access_token_encrypted = $2,
    refresh_token_encrypted = COALESCE($3, refresh_token_encrypted),
    encryption_nonce = $4,
    expires_at = $5,
    updated_at = NOW()
WHERE id = $1
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
