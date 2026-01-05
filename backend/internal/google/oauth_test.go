package google

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/people/v1"
)

// TestScopes verifies that all required OAuth scopes are present
// This test ensures the bug fix (missing openid, email, profile scopes) doesn't regress
func TestScopes(t *testing.T) {
	// Verify all OpenID Connect scopes are present
	assert.Contains(t, Scopes, "openid", "openid scope is required for OAuth user identification")
	assert.Contains(t, Scopes, "email", "email scope is required to get user email address")
	assert.Contains(t, Scopes, "profile", "profile scope is required to get user profile info")

	// Verify Google API scopes are present
	assert.Contains(t, Scopes, gmail.GmailReadonlyScope, "Gmail readonly scope is required")
	assert.Contains(t, Scopes, calendar.CalendarReadonlyScope, "Calendar readonly scope is required")
	assert.Contains(t, Scopes, people.ContactsReadonlyScope, "Contacts readonly scope is required")

	// Verify we have exactly 6 scopes (no more, no less)
	assert.Len(t, Scopes, 6, "Should have exactly 6 scopes")
}

// TestScopes_Order verifies the scopes are in the expected order
// OpenID scopes should come first, then API scopes
func TestScopes_Order(t *testing.T) {
	require.Len(t, Scopes, 6, "Expected 6 scopes")

	// OpenID scopes should be first
	assert.Equal(t, "openid", Scopes[0], "openid should be first")
	assert.Equal(t, "email", Scopes[1], "email should be second")
	assert.Equal(t, "profile", Scopes[2], "profile should be third")

	// API scopes should follow
	assert.Equal(t, gmail.GmailReadonlyScope, Scopes[3])
	assert.Equal(t, calendar.CalendarReadonlyScope, Scopes[4])
	assert.Equal(t, people.ContactsReadonlyScope, Scopes[5])
}

// TestGetAuthURL_IncludesConsentPrompt verifies that the auth URL includes prompt=consent
// This ensures users always see the consent screen with updated scopes
func TestGetAuthURL_IncludesConsentPrompt(t *testing.T) {
	// Create a minimal OAuth config for testing
	config := &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/callback",
		Scopes:       Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		},
	}

	service := &OAuthService{
		config: config,
	}

	// Generate auth URL
	state := "test-state-12345"
	authURL := service.GetAuthURL(state)

	// Parse the URL
	parsedURL, err := url.Parse(authURL)
	require.NoError(t, err, "Auth URL should be valid")

	// Extract query parameters
	query := parsedURL.Query()

	// Verify prompt=consent is present
	assert.Equal(t, "consent", query.Get("prompt"), "Auth URL must include prompt=consent parameter")

	// Verify access_type=offline is present (for refresh tokens)
	assert.Equal(t, "offline", query.Get("access_type"), "Auth URL must include access_type=offline for refresh tokens")

	// Verify state is included
	assert.Equal(t, state, query.Get("state"), "Auth URL must include the state parameter")

	// Verify client_id is included
	assert.Equal(t, "test-client-id", query.Get("client_id"), "Auth URL must include client_id")

	// Verify redirect_uri is included
	assert.Equal(t, "http://localhost:8080/callback", query.Get("redirect_uri"), "Auth URL must include redirect_uri")

	// Verify scopes are included
	scopeParam := query.Get("scope")
	assert.NotEmpty(t, scopeParam, "Auth URL must include scope parameter")

	// Verify all required scopes are in the scope parameter
	for _, scope := range Scopes {
		assert.Contains(t, scopeParam, scope, "Scope parameter must include %s", scope)
	}
}

// TestGetAuthURL_ScopeFormatting verifies scopes are properly formatted in the URL
func TestGetAuthURL_ScopeFormatting(t *testing.T) {
	config := &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/callback",
		Scopes:       Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		},
	}

	service := &OAuthService{
		config: config,
	}

	authURL := service.GetAuthURL("test-state")
	parsedURL, err := url.Parse(authURL)
	require.NoError(t, err)

	scopeParam := parsedURL.Query().Get("scope")

	// Scopes should be space-separated when URL-decoded
	// The URL encoding might use + or %20 for spaces
	decodedScopes, err := url.QueryUnescape(scopeParam)
	require.NoError(t, err)

	// Split by space and verify we have all scopes
	scopeParts := strings.Fields(decodedScopes)
	assert.GreaterOrEqual(t, len(scopeParts), 6, "Should have at least 6 scopes in the URL")
}

// TestGenerateState verifies that state generation produces unique, secure values
func TestGenerateState(t *testing.T) {
	// Generate multiple states
	states := make(map[string]bool)
	for i := 0; i < 100; i++ {
		state, err := GenerateState()
		require.NoError(t, err, "State generation should not fail")
		require.NotEmpty(t, state, "State should not be empty")

		// Verify state is unique
		assert.False(t, states[state], "State should be unique (collision detected)")
		states[state] = true

		// Verify state is long enough (32 bytes = ~43-44 chars in base64)
		assert.GreaterOrEqual(t, len(state), 40, "State should be at least 40 characters")

		// Verify state only contains valid base64 characters
		// URL-safe base64 uses: A-Z, a-z, 0-9, -, _, and optionally =
		for _, ch := range state {
			valid := (ch >= 'A' && ch <= 'Z') ||
				(ch >= 'a' && ch <= 'z') ||
				(ch >= '0' && ch <= '9') ||
				ch == '-' || ch == '_' || ch == '='
			assert.True(t, valid, "State should only contain valid base64 characters")
		}
	}
}

// TestProviderName verifies the provider constant
func TestProviderName(t *testing.T) {
	assert.Equal(t, "google", ProviderName, "Provider name should be 'google'")
}

// TestOAuthService_ConfigConstruction verifies OAuth config is created with correct scopes
func TestOAuthService_ConfigConstruction(t *testing.T) {
	// This test verifies that when an OAuthService is created, the config has the right scopes
	config := &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/callback",
		Scopes:       Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		},
	}

	service := &OAuthService{
		config: config,
	}

	// Verify the service's config has all required scopes
	assert.Len(t, service.config.Scopes, 6, "Config should have 6 scopes")
	assert.Contains(t, service.config.Scopes, "openid")
	assert.Contains(t, service.config.Scopes, "email")
	assert.Contains(t, service.config.Scopes, "profile")
	assert.Contains(t, service.config.Scopes, gmail.GmailReadonlyScope)
	assert.Contains(t, service.config.Scopes, calendar.CalendarReadonlyScope)
	assert.Contains(t, service.config.Scopes, people.ContactsReadonlyScope)
}
