-- OAuth Credentials Storage
-- Stores OAuth2 tokens for external services (Google, future: Microsoft, etc.)
-- Supports multiple accounts per provider (e.g., personal + work Google accounts)

CREATE TABLE oauth_credential (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider TEXT NOT NULL,                      -- 'google', future: 'microsoft', 'apple'
    account_id TEXT NOT NULL,                    -- User's email/identifier from the provider
    account_name TEXT,                           -- Display name from the provider

    -- Encrypted token data (AES-256-GCM)
    access_token_encrypted BYTEA NOT NULL,
    refresh_token_encrypted BYTEA,
    encryption_nonce BYTEA NOT NULL,             -- Nonce for AES-GCM decryption

    -- Token metadata (not sensitive)
    token_type TEXT DEFAULT 'Bearer',
    expires_at TIMESTAMPTZ,                      -- Access token expiry
    scopes TEXT[],                               -- Granted OAuth scopes

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Unique constraint: one credential per provider + account combination
CREATE UNIQUE INDEX idx_oauth_credential_provider_account ON oauth_credential(provider, account_id);

-- Index for listing credentials by provider
CREATE INDEX idx_oauth_credential_provider ON oauth_credential(provider);

-- Index for finding expired tokens that need refresh
CREATE INDEX idx_oauth_credential_expires ON oauth_credential(expires_at) WHERE expires_at IS NOT NULL;
