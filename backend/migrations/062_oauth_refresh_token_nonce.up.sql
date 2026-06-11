-- Give the refresh token its own AES-GCM nonce.
--
-- Previously the refresh token was encrypted with the same nonce as the access
-- token (encryption_nonce). Reusing a nonce with the same key in AES-GCM leaks
-- keystream and is a security risk. This column lets each field carry its own
-- nonce. A NULL value means the row predates this change and its refresh token
-- still uses the shared encryption_nonce (legacy format); such rows are upgraded
-- to the new format the next time their tokens are written.
ALTER TABLE oauth_credential ADD COLUMN refresh_token_nonce BYTEA;
