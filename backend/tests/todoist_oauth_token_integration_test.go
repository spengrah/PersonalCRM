package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTodoistOAuthTestService wires a Todoist OAuthService against the shared
// test DB and returns the service plus the repository so tests can inspect
// the stored (encrypted) row directly. The token/userinfo endpoints are
// package-level vars (unlike Google's per-instance setters), so callers must
// point them at a fake server and restore the originals in t.Cleanup.
func newTodoistOAuthTestService(t *testing.T) (*todoist.OAuthService, *repository.OAuthRepository, context.Context) {
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
	svc, err := todoist.NewOAuthService(cfg, repo, nil)
	require.NoError(t, err)

	return svc, repo, ctx
}

// fakeTodoistEndpoints stands in for Todoist's token and userinfo endpoints.
// The token handler returns whatever access token is currently configured, so
// a test can rotate it between calls to exercise re-exchange persistence.
type fakeTodoistEndpoints struct {
	server      *httptest.Server
	accessToken string
}

func newFakeTodoistEndpoints(t *testing.T, userID, email string) *fakeTodoistEndpoints {
	t.Helper()
	f := &fakeTodoistEndpoints{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": f.accessToken,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": userID, "email": email})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// withTodoistEndpoints points the package-level Todoist token/userinfo
// endpoint vars at fake's server and restores the originals in t.Cleanup.
// ExchangeCode reads these vars directly (no per-instance setter exists on
// OAuthService, unlike Google's SetTokenEndpoint/SetUserInfoEndpoint), so
// tests in this file must not run in parallel with each other.
func withTodoistEndpoints(t *testing.T, fake *fakeTodoistEndpoints) {
	t.Helper()
	origToken := todoist.TokenEndpoint
	origUserInfo := todoist.UserInfoEndpoint
	todoist.TokenEndpoint = fake.server.URL + "/token"
	todoist.UserInfoEndpoint = fake.server.URL + "/user"
	t.Cleanup(func() {
		todoist.TokenEndpoint = origToken
		todoist.UserInfoEndpoint = origUserInfo
	})
}

// TestTodoistOAuth_ExchangeRoundTrip verifies that a token exchanged via the
// fake endpoint is stored encrypted (never as plaintext) and decrypts back to
// the same value, under a populated per-credential nonce. Todoist tokens
// never carry a refresh token (repo.storeToken always passes a nil refresh
// ciphertext/nonce), so unlike Google there is only one token to store per
// credential; the round trip proves that single stored token is encrypted at
// rest under its own nonce.
// spec: SET-009
func TestTodoistOAuth_ExchangeRoundTrip(t *testing.T) {
	userID := syntheticNS(t)
	email := userID + "@example.test"

	svc, repo, ctx := newTodoistOAuthTestService(t)
	fake := newFakeTodoistEndpoints(t, userID, email)
	fake.accessToken = "fake-todoist-access-token-rt"
	withTodoistEndpoints(t, fake)

	t.Cleanup(func() {
		if cred, err := repo.GetByProviderAndAccount(context.Background(), todoist.ProviderName, userID); err == nil {
			_ = repo.Delete(context.Background(), cred.ID)
		}
	})

	status, err := svc.ExchangeCode(ctx, "fake-auth-code")
	require.NoError(t, err)
	require.Equal(t, userID, status.AccountID)

	// Decrypt fidelity: the decrypted token matches what the endpoint returned.
	accessToken, err := svc.GetAccessToken(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "fake-todoist-access-token-rt", accessToken)

	// Encrypted at rest: the stored ciphertext must never equal the plaintext
	// wire bytes, and a populated nonce must back the encryption.
	cred, err := repo.GetByProviderAndAccount(ctx, todoist.ProviderName, userID)
	require.NoError(t, err)
	require.NotEmpty(t, cred.AccessTokenEncrypted)
	assert.False(t, bytes.Equal(cred.AccessTokenEncrypted, []byte("fake-todoist-access-token-rt")),
		"access token must not be stored as plaintext")
	require.NotEmpty(t, cred.EncryptionNonce, "access token must be stored under a nonce")

	// Todoist never issues a refresh token, so there is nothing to store
	// under a second nonce here (contrast with Google's distinct-nonces case).
	assert.Empty(t, cred.RefreshTokenEncrypted, "Todoist tokens carry no refresh token to encrypt")
	assert.Empty(t, cred.RefreshTokenNonce, "Todoist tokens carry no refresh-token nonce")
}

// TestTodoistOAuth_ReExchangeUsesDistinctNonce verifies the Todoist analogue
// of Google's "distinct nonces" guarantee: since Todoist has only one token
// per credential (no refresh token), the per-token-distinct-nonce invariant
// instead applies across successive writes of that one token — a re-exchange
// that rotates the access token must mint a fresh nonce rather than reusing
// the previous write's nonce (nonce reuse under AES-GCM breaks
// confidentiality outright).
// spec: SET-009
func TestTodoistOAuth_ReExchangeUsesDistinctNonce(t *testing.T) {
	userID := syntheticNS(t)
	email := userID + "@example.test"

	svc, repo, ctx := newTodoistOAuthTestService(t)
	fake := newFakeTodoistEndpoints(t, userID, email)
	withTodoistEndpoints(t, fake)

	t.Cleanup(func() {
		if cred, err := repo.GetByProviderAndAccount(context.Background(), todoist.ProviderName, userID); err == nil {
			_ = repo.Delete(context.Background(), cred.ID)
		}
	})

	fake.accessToken = "first-todoist-access-token"
	_, err := svc.ExchangeCode(ctx, "first-code")
	require.NoError(t, err)

	first, err := repo.GetByProviderAndAccount(ctx, todoist.ProviderName, userID)
	require.NoError(t, err)
	require.NotEmpty(t, first.EncryptionNonce)

	fake.accessToken = "second-todoist-access-token"
	_, err = svc.ExchangeCode(ctx, "second-code")
	require.NoError(t, err)

	second, err := repo.GetByProviderAndAccount(ctx, todoist.ProviderName, userID)
	require.NoError(t, err)
	require.NotEmpty(t, second.EncryptionNonce)

	assert.False(t, bytes.Equal(first.EncryptionNonce, second.EncryptionNonce),
		"each write of the token must use its own nonce, not reuse the previous write's nonce")

	// Fidelity: the second exchange's decrypted value reflects the rotation.
	accessToken, err := svc.GetAccessToken(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "second-todoist-access-token", accessToken)
}
