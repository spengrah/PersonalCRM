package todoist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScopes verifies that all required OAuth scopes are present
func TestScopes(t *testing.T) {
	assert.Contains(t, Scopes, "data:read_write", "data:read_write scope is required for Sync API")
	assert.Contains(t, Scopes, "data:delete", "data:delete scope is required for future deletion flows")
	assert.Len(t, Scopes, 2, "Should have exactly 2 scopes")
}

// TestScopes_Order verifies the scopes are in the expected order
func TestScopes_Order(t *testing.T) {
	require.Len(t, Scopes, 2, "Expected 2 scopes")
	assert.Equal(t, "data:read_write", Scopes[0], "data:read_write should be first")
	assert.Equal(t, "data:delete", Scopes[1], "data:delete should be second")
}

// TestProviderName verifies the provider constant
func TestProviderName(t *testing.T) {
	assert.Equal(t, "todoist", ProviderName, "Provider name should be 'todoist'")
}

// TestOAuthEndpoints verifies the OAuth endpoint constants
func TestOAuthEndpoints(t *testing.T) {
	assert.Equal(t, "https://app.todoist.com/oauth/authorize", AuthorizationEndpoint)
	assert.Equal(t, "https://api.todoist.com/oauth/access_token", TokenEndpoint)
	assert.Equal(t, "https://api.todoist.com/api/v1/revoke", RevokeEndpoint)
	assert.Equal(t, "https://api.todoist.com/api/v1/user", UserInfoEndpoint)
}

// TestGetAuthURL_IncludesCorrectParams verifies auth URL has correct parameters
func TestGetAuthURL_IncludesCorrectParams(t *testing.T) {
	service := &OAuthService{
		clientID:    "test-client-id",
		redirectURL: "http://localhost:8080/api/v1/auth/todoist/callback",
	}

	state := "test-state-12345"
	authURL := service.GetAuthURL(state)

	parsedURL, err := url.Parse(authURL)
	require.NoError(t, err, "Auth URL should be valid")

	// Verify the base URL is correct
	assert.Equal(t, "app.todoist.com", parsedURL.Host, "Host should be app.todoist.com")
	assert.Equal(t, "/oauth/authorize", parsedURL.Path, "Path should be /oauth/authorize")

	// Extract query parameters
	query := parsedURL.Query()

	// Verify client_id is present
	assert.Equal(t, "test-client-id", query.Get("client_id"), "Auth URL must include client_id")

	// Verify state is included
	assert.Equal(t, state, query.Get("state"), "Auth URL must include the state parameter")

	// Verify scope is comma-separated (per Todoist API spec)
	scopeParam := query.Get("scope")
	assert.NotEmpty(t, scopeParam, "Auth URL must include scope parameter")
	assert.Equal(t, "data:read_write,data:delete", scopeParam, "Scopes should be comma-separated")
}

// TestGetAuthURL_ScopeFormatting verifies scopes are comma-separated per Todoist API spec
func TestGetAuthURL_ScopeFormatting(t *testing.T) {
	service := &OAuthService{
		clientID:    "test-client-id",
		redirectURL: "http://localhost:8080/callback",
	}

	authURL := service.GetAuthURL("test-state")
	parsedURL, err := url.Parse(authURL)
	require.NoError(t, err)

	scopeParam := parsedURL.Query().Get("scope")

	// Todoist requires comma-separated scopes (not space-separated like Google)
	assert.Contains(t, scopeParam, ",", "Scopes should be comma-separated for Todoist")
	assert.NotContains(t, scopeParam, " ", "Scopes should not be space-separated for Todoist")

	// Verify both scopes are present
	assert.Contains(t, scopeParam, "data:read_write")
	assert.Contains(t, scopeParam, "data:delete")
}

// TestExchangeCodeForToken_Success tests successful token exchange HTTP call
func TestExchangeCodeForToken_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

		err := r.ParseForm()
		require.NoError(t, err)

		assert.Equal(t, "test-client-id", r.Form.Get("client_id"))
		assert.Equal(t, "test-client-secret", r.Form.Get("client_secret"))
		assert.Equal(t, "test-auth-code", r.Form.Get("code"))
		assert.Equal(t, "http://localhost:8080/callback", r.Form.Get("redirect_uri"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	service := &OAuthService{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		redirectURL:  "http://localhost:8080/callback",
		httpClient:   server.Client(),
	}

	ctx := context.Background()
	token, err := service.exchangeCodeForTokenWithURL(ctx, "test-auth-code", server.URL)
	require.NoError(t, err)
	assert.Equal(t, "test-access-token", token.AccessToken)
	assert.Equal(t, "Bearer", token.TokenType)
}

// TestExchangeCodeForToken_Error tests handling of token exchange errors
func TestExchangeCodeForToken_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
	}))
	defer server.Close()

	service := &OAuthService{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		redirectURL:  "http://localhost:8080/callback",
		httpClient:   server.Client(),
	}

	ctx := context.Background()
	_, err := service.exchangeCodeForTokenWithURL(ctx, "invalid-code", server.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token exchange failed")
}

// TestGetUserInfo_Success tests successful user info fetch
func TestGetUserInfo_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":    "12345",
			"email": "test@example.com",
		})
	}))
	defer server.Close()

	service := &OAuthService{
		httpClient: server.Client(),
	}

	ctx := context.Background()
	userInfo, err := service.getUserInfoWithURL(ctx, "test-access-token", server.URL)
	require.NoError(t, err)
	assert.Equal(t, "12345", userInfo.ID)
	assert.Equal(t, "test@example.com", userInfo.Email)
}

// TestGetUserInfo_Error tests handling of user info fetch errors
func TestGetUserInfo_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "invalid_token"}`))
	}))
	defer server.Close()

	service := &OAuthService{
		httpClient: server.Client(),
	}

	ctx := context.Background()
	_, err := service.getUserInfoWithURL(ctx, "invalid-token", server.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "user info request failed")
}

// TestRevokeToken_Success tests successful token revocation
func TestRevokeToken_Success(t *testing.T) {
	var receivedAuth string
	var receivedToken string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

		receivedAuth = r.Header.Get("Authorization")

		err := r.ParseForm()
		require.NoError(t, err)
		receivedToken = r.Form.Get("token")
		assert.Equal(t, "access_token", r.Form.Get("token_type_hint"))

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	service := &OAuthService{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		httpClient:   server.Client(),
	}

	ctx := context.Background()
	err := service.revokeTokenWithURL(ctx, "test-access-token", server.URL)
	require.NoError(t, err)

	// Verify Basic auth header
	assert.True(t, strings.HasPrefix(receivedAuth, "Basic "), "Should use Basic auth")
	assert.Equal(t, "test-access-token", receivedToken)
}

// TestRevokeToken_Error tests handling of revoke errors
func TestRevokeToken_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "server_error"}`))
	}))
	defer server.Close()

	service := &OAuthService{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		httpClient:   server.Client(),
	}

	ctx := context.Background()
	err := service.revokeTokenWithURL(ctx, "test-access-token", server.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "revoke failed")
}

// TestRevokeToken_NoContent tests that 204 No Content is accepted
func TestRevokeToken_NoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	service := &OAuthService{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		httpClient:   server.Client(),
	}

	ctx := context.Background()
	err := service.revokeTokenWithURL(ctx, "test-access-token", server.URL)
	require.NoError(t, err)
}
