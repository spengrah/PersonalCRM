package tests

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGoogleOAuthTestService wires a Google OAuthService against the shared test
// DB and returns the service plus the repository so tests can inspect the stored
// (encrypted) row directly. The HTTP client and userinfo endpoint are pointed at
// the caller-supplied test server URL.
func newGoogleOAuthTestService(t *testing.T) (*google.OAuthService, *repository.OAuthRepository, *config.Config, context.Context) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewOAuthRepository(database.Queries)
	svc, err := google.NewOAuthService(cfg, repo, nil)
	require.NoError(t, err)

	return svc, repo, cfg, ctx
}

// fakeGoogleEndpoints stands in for Google's token and userinfo endpoints. The
// token handler returns whatever access/refresh tokens are currently configured,
// so a test can rotate them between calls to exercise refresh persistence.
type fakeGoogleEndpoints struct {
	server      *httptest.Server
	accessToken string
	refreshTok  string
	tokenCalls  int
}

func newFakeGoogleEndpoints(t *testing.T, email, name string) *fakeGoogleEndpoints {
	t.Helper()
	f := &fakeGoogleEndpoints{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"access_token": f.accessToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if f.refreshTok != "" {
			resp["refresh_token"] = f.refreshTok
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"email": email, "name": name})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// sealWithKey encrypts plaintext with the given hex key and nonce using raw
// AES-256-GCM. It fabricates a legacy credential row whose refresh token shares
// the access token's nonce, exactly as the previous code stored it.
func sealWithKey(t *testing.T, hexKey, plaintext string, nonce []byte) []byte {
	t.Helper()
	key, err := hex.DecodeString(hexKey)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return gcm.Seal(nil, nonce, []byte(plaintext), nil)
}

// TestGoogleOAuth_ExchangeRoundTrip verifies that a token exchanged via the fake
// endpoint is stored encrypted and decrypts back to the same values, and that the
// access and refresh tokens are stored under distinct nonces.
// spec: SET-009
func TestGoogleOAuth_ExchangeRoundTrip(t *testing.T) {
	t.Parallel()

	email := syntheticNS(t) + "@example.test"
	svc, repo, _, ctx := newGoogleOAuthTestService(t)
	fake := newFakeGoogleEndpoints(t, email, "Round Trip")
	fake.accessToken = "fake-access-token-rt"
	fake.refreshTok = "fake-refresh-token-rt"

	svc.SetHTTPClient(fake.server.Client())
	svc.SetTokenEndpoint(fake.server.URL + "/token")
	svc.SetUserInfoEndpoint(fake.server.URL + "/userinfo")

	t.Cleanup(func() {
		if cred, err := repo.GetByProviderAndAccount(context.Background(), google.ProviderName, email); err == nil {
			_ = repo.Delete(context.Background(), cred.ID)
		}
	})

	status, err := svc.ExchangeCode(ctx, "fake-auth-code")
	require.NoError(t, err)
	require.Equal(t, email, status.AccountID)

	// Decrypt fidelity: the decrypted token matches what the endpoint returned.
	token, err := svc.GetTokenForAccount(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, "fake-access-token-rt", token.AccessToken)
	assert.Equal(t, "fake-refresh-token-rt", token.RefreshToken)

	// Regression: the two stored ciphertexts must never share a nonce.
	cred, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
	require.NoError(t, err)
	require.NotEmpty(t, cred.RefreshTokenEncrypted)
	require.NotEmpty(t, cred.RefreshTokenNonce, "new format must store a dedicated refresh-token nonce")
	assert.NotEqual(t, cred.EncryptionNonce, cred.RefreshTokenNonce, "access and refresh tokens must use distinct nonces")
}

// TestGoogleOAuth_RefreshPersistsRotatedToken verifies that a refresh performed by
// the token source persists the new access token and a rotated refresh token.
func TestGoogleOAuth_RefreshPersistsRotatedToken(t *testing.T) {
	t.Parallel()

	email := syntheticNS(t) + "@example.test"
	svc, repo, _, ctx := newGoogleOAuthTestService(t)
	fake := newFakeGoogleEndpoints(t, email, "Refresh Rotate")
	svc.SetHTTPClient(fake.server.Client())
	svc.SetTokenEndpoint(fake.server.URL + "/token")
	svc.SetUserInfoEndpoint(fake.server.URL + "/userinfo")

	t.Cleanup(func() {
		if cred, err := repo.GetByProviderAndAccount(context.Background(), google.ProviderName, email); err == nil {
			_ = repo.Delete(context.Background(), cred.ID)
		}
	})

	// Seed an already-expired credential with a refresh token so the token source
	// is forced to refresh on first use. Encrypt through the real encryptor by way
	// of an exchange whose returned token we then expire in the DB.
	fake.accessToken = "stale-access-token"
	fake.refreshTok = "original-refresh-token"
	_, err := svc.ExchangeCode(ctx, "seed-code")
	require.NoError(t, err)

	cred, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
	require.NoError(t, err)
	expired := accelerated.GetCurrentTime().Add(-1 * time.Hour)
	_, err = repo.UpdateTokens(ctx, cred.ID, repository.UpdateOAuthTokensRequest{
		AccessTokenEncrypted:  cred.AccessTokenEncrypted,
		RefreshTokenEncrypted: cred.RefreshTokenEncrypted,
		RefreshTokenNonce:     cred.RefreshTokenNonce,
		EncryptionNonce:       cred.EncryptionNonce,
		ExpiresAt:             &expired,
	})
	require.NoError(t, err)

	// Configure the endpoint to rotate both tokens on the refresh call.
	fake.accessToken = "rotated-access-token"
	fake.refreshTok = "rotated-refresh-token"
	callsBefore := fake.tokenCalls

	// The client refreshes lazily on its first request; the expired credential
	// forces a refresh against the fake token endpoint.
	client, err := svc.GetClientForAccount(ctx, email)
	require.NoError(t, err)
	resp, err := client.Get(fake.server.URL + "/userinfo")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Greater(t, fake.tokenCalls, callsBefore, "a refresh should have hit the token endpoint")

	// The rotated tokens must be persisted to the DB.
	stored, err := svc.GetTokenForAccount(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, "rotated-access-token", stored.AccessToken)
	assert.Equal(t, "rotated-refresh-token", stored.RefreshToken)

	// Persisted refresh token keeps its own nonce.
	credAfter, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
	require.NoError(t, err)
	require.NotEmpty(t, credAfter.RefreshTokenNonce)
	assert.NotEqual(t, credAfter.EncryptionNonce, credAfter.RefreshTokenNonce)
}

// TestGoogleOAuth_LegacyRowDecryptsAndUpgrades verifies a row stored in the old
// shared-nonce format still decrypts, and is upgraded to the per-field nonce
// format the next time its tokens are written.
func TestGoogleOAuth_LegacyRowDecryptsAndUpgrades(t *testing.T) {
	t.Parallel()

	email := syntheticNS(t) + "@example.test"
	svc, repo, cfg, ctx := newGoogleOAuthTestService(t)

	t.Cleanup(func() {
		if cred, err := repo.GetByProviderAndAccount(context.Background(), google.ProviderName, email); err == nil {
			_ = repo.Delete(context.Background(), cred.ID)
		}
	})

	// Build a legacy row: access and refresh tokens encrypted under the same
	// nonce, with refresh_token_nonce left NULL.
	const accessPlain = "legacy-access-token"
	const refreshPlain = "legacy-refresh-token"
	sharedNonce := make([]byte, 12)
	for i := range sharedNonce {
		sharedNonce[i] = byte(i + 1)
	}
	accessCipher := sealWithKey(t, cfg.External.TokenEncryptionKey, accessPlain, sharedNonce)
	refreshCipher := sealWithKey(t, cfg.External.TokenEncryptionKey, refreshPlain, sharedNonce)

	_, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
		Provider:              google.ProviderName,
		AccountID:             email,
		AccessTokenEncrypted:  accessCipher,
		RefreshTokenEncrypted: refreshCipher,
		RefreshTokenNonce:     nil, // legacy format
		EncryptionNonce:       sharedNonce,
		TokenType:             "Bearer",
		Scopes:                google.Scopes,
	})
	require.NoError(t, err)

	// Legacy row decrypts correctly via the shared-nonce fallback.
	token, err := svc.GetTokenForAccount(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, accessPlain, token.AccessToken)
	assert.Equal(t, refreshPlain, token.RefreshToken)

	legacy, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
	require.NoError(t, err)
	require.Empty(t, legacy.RefreshTokenNonce, "fixture should start in legacy (NULL nonce) format")

	// Rewriting the tokens upgrades the row to the new per-field nonce format.
	fake := newFakeGoogleEndpoints(t, email, "Legacy Upgrade")
	fake.accessToken = "upgraded-access-token"
	fake.refreshTok = "upgraded-refresh-token"
	svc.SetHTTPClient(fake.server.Client())
	svc.SetTokenEndpoint(fake.server.URL + "/token")
	svc.SetUserInfoEndpoint(fake.server.URL + "/userinfo")

	_, err = svc.ExchangeCode(ctx, "upgrade-code")
	require.NoError(t, err)

	upgraded, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
	require.NoError(t, err)
	require.NotEmpty(t, upgraded.RefreshTokenNonce, "rewrite must populate a dedicated refresh-token nonce")
	assert.NotEqual(t, upgraded.EncryptionNonce, upgraded.RefreshTokenNonce)

	stored, err := svc.GetTokenForAccount(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, "upgraded-access-token", stored.AccessToken)
	assert.Equal(t, "upgraded-refresh-token", stored.RefreshToken)
}

// TestGoogleOAuth_LegacyRow_RefreshlessWriteKeepsDecryptable verifies that a
// write which preserves an existing legacy refresh ciphertext (because no new
// refresh token is supplied) captures the legacy shared nonce into the dedicated
// column. Without that capture such a write rotates encryption_nonce while
// leaving refresh_token_nonce NULL, so the legacy decrypt fallback would read
// the new access-token nonce and the preserved refresh token would become
// permanently undecryptable.
func TestGoogleOAuth_LegacyRow_RefreshlessWriteKeepsDecryptable(t *testing.T) {
	t.Parallel()

	svc, repo, cfg, ctx := newGoogleOAuthTestService(t)

	const refreshPlain = "legacy-refresh-token-preserved"

	// seedLegacyRow stores a row in the old format: refresh token sealed with the
	// access token's nonce and refresh_token_nonce left NULL.
	seedLegacyRow := func(t *testing.T, email string) {
		t.Helper()
		sharedNonce := make([]byte, 12)
		for i := range sharedNonce {
			sharedNonce[i] = byte(i + 7)
		}
		accessCipher := sealWithKey(t, cfg.External.TokenEncryptionKey, "legacy-access-token", sharedNonce)
		refreshCipher := sealWithKey(t, cfg.External.TokenEncryptionKey, refreshPlain, sharedNonce)

		_, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
			Provider:              google.ProviderName,
			AccountID:             email,
			AccessTokenEncrypted:  accessCipher,
			RefreshTokenEncrypted: refreshCipher,
			RefreshTokenNonce:     nil, // legacy format
			EncryptionNonce:       sharedNonce,
			TokenType:             "Bearer",
			Scopes:                google.Scopes,
		})
		require.NoError(t, err)

		t.Cleanup(func() {
			if cred, err := repo.GetByProviderAndAccount(context.Background(), google.ProviderName, email); err == nil {
				_ = repo.Delete(context.Background(), cred.ID)
			}
		})
	}

	// Re-exchange where Google omits refresh_token: hits the upsert conflict
	// branch with a NULL refresh ciphertext.
	t.Run("UpsertConflictBranch", func(t *testing.T) {
		email := syntheticNS(t) + "@example.test"
		seedLegacyRow(t, email)

		fake := newFakeGoogleEndpoints(t, email, "Legacy Refreshless Upsert")
		fake.accessToken = "reexchanged-access-token"
		fake.refreshTok = "" // omitted from the token response
		svc.SetHTTPClient(fake.server.Client())
		svc.SetTokenEndpoint(fake.server.URL + "/token")
		svc.SetUserInfoEndpoint(fake.server.URL + "/userinfo")

		_, err := svc.ExchangeCode(ctx, "reexchange-code")
		require.NoError(t, err)

		token, err := svc.GetTokenForAccount(ctx, email)
		require.NoError(t, err, "preserved legacy refresh token must stay decryptable after a refreshless upsert")
		assert.Equal(t, "reexchanged-access-token", token.AccessToken)
		assert.Equal(t, refreshPlain, token.RefreshToken)

		cred, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
		require.NoError(t, err)
		assert.NotEmpty(t, cred.RefreshTokenNonce, "legacy shared nonce must be captured into the dedicated column")
	})

	// Token refresh where the provider does not rotate the refresh token: hits
	// UpdateOAuthCredentialTokens with a NULL refresh ciphertext.
	t.Run("TokensUpdate", func(t *testing.T) {
		email := syntheticNS(t) + "@example.test"
		seedLegacyRow(t, email)

		cred, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
		require.NoError(t, err)

		encryptor, err := crypto.NewTokenEncryptor(cfg.External.TokenEncryptionKey)
		require.NoError(t, err)
		newAccessCipher, newNonce, err := encryptor.Encrypt("refreshed-access-token")
		require.NoError(t, err)

		_, err = repo.UpdateTokens(ctx, cred.ID, repository.UpdateOAuthTokensRequest{
			AccessTokenEncrypted:  newAccessCipher,
			RefreshTokenEncrypted: nil, // refresh token not rotated
			RefreshTokenNonce:     nil,
			EncryptionNonce:       newNonce,
		})
		require.NoError(t, err)

		token, err := svc.GetTokenForAccount(ctx, email)
		require.NoError(t, err, "preserved legacy refresh token must stay decryptable after a refreshless tokens update")
		assert.Equal(t, "refreshed-access-token", token.AccessToken)
		assert.Equal(t, refreshPlain, token.RefreshToken)

		credAfter, err := repo.GetByProviderAndAccount(ctx, google.ProviderName, email)
		require.NoError(t, err)
		assert.NotEmpty(t, credAfter.RefreshTokenNonce, "legacy shared nonce must be captured into the dedicated column")
	})
}
