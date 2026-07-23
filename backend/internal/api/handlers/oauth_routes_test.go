package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTodoistOAuthService is a minimal mock of todoist.OAuthServiceInterface.
// Registration tests only need HasTodoistOAuth() to flip true, so every
// method is a stub; the Google twin reuses MockOAuthService from
// oauth_test.go (same package).
type mockTodoistOAuthService struct{}

var _ todoist.OAuthServiceInterface = (*mockTodoistOAuthService)(nil)

func (m *mockTodoistOAuthService) GetAuthURL(state string) string {
	return "https://todoist.com/oauth/authorize?state=" + state
}

func (m *mockTodoistOAuthService) ExchangeCode(ctx context.Context, code string) (*repository.OAuthCredentialStatus, error) {
	return nil, errors.New("not implemented")
}

func (m *mockTodoistOAuthService) ListAccounts(ctx context.Context) ([]repository.OAuthCredentialStatus, error) {
	return nil, nil
}

func (m *mockTodoistOAuthService) GetAccountStatus(ctx context.Context, id uuid.UUID) (*repository.OAuthCredentialStatus, error) {
	return nil, db.ErrNotFound
}

func (m *mockTodoistOAuthService) RevokeAccount(ctx context.Context, id uuid.UUID) error {
	return nil
}

const oauthRoutesTestAPIKey = "oauth-routes-test-key-2f8c"

// newOAuthTestRouter builds a router wired exactly the way
// cmd/crm-api/routes.go wires the OAuth registrars: the callback
// registrar goes on the bare engine BEFORE any auth middleware, then the
// /api/v1 group gets auth.APIKeyMiddleware and the auth-URL/account
// registrar is mounted inside it. GoogleEnabled mirrors run()'s
// googleOAuthService != nil check (the same value gates both registrars);
// the Todoist gate is the handler's HasTodoistOAuth().
func newOAuthTestRouter(googleEnabled, todoistEnabled bool) *gin.Engine {
	var googleSvc google.OAuthServiceInterface
	if googleEnabled {
		googleSvc = &MockOAuthService{}
	}
	handler := NewOAuthHandler(googleSvc, "http://localhost:3000")
	if todoistEnabled {
		handler.SetTodoistOAuth(&mockTodoistOAuthService{})
	}
	deps := OAuthCallbackDeps{
		Handler:       handler,
		GoogleEnabled: googleEnabled,
	}

	cfg := &config.Config{
		External: config.ExternalConfig{APIKey: oauthRoutesTestAPIKey},
	}

	router := gin.New()
	// Callback routes: bare router, no auth (provider redirects cannot
	// carry the global API key).
	RegisterOAuthCallbackRoutes(router, deps)
	// Everything else: inside the API-key-protected group.
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	RegisterOAuthRoutes(v1, deps)
	return router
}

func serveOAuthTestRequest(router *gin.Engine, method, path string, withKey bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if withKey {
		req.Header.Set("X-API-Key", oauthRoutesTestAPIKey)
	}
	router.ServeHTTP(w, req)
	return w
}

// oauthRouteSet returns "METHOD path" for every registered route under
// /api/v1/auth (both the bare callback routes and the grouped ones).
func oauthRouteSet(router *gin.Engine) []string {
	var routes []string
	for _, route := range router.Routes() {
		if len(route.Path) >= len("/api/v1/auth") && route.Path[:len("/api/v1/auth")] == "/api/v1/auth" {
			routes = append(routes, route.Method+" "+route.Path)
		}
	}
	return routes
}

var googleOAuthRouteSet = []string{
	"GET /api/v1/auth/google/callback",
	"GET /api/v1/auth/google",
	"GET /api/v1/auth/google/accounts",
	"GET /api/v1/auth/google/accounts/:id/status",
	"POST /api/v1/auth/google/accounts/:id/revoke",
}

var todoistOAuthRouteSet = []string{
	"GET /api/v1/auth/todoist/callback",
	"GET /api/v1/auth/todoist",
	"GET /api/v1/auth/todoist/accounts",
	"GET /api/v1/auth/todoist/accounts/:id/status",
	"POST /api/v1/auth/todoist/accounts/:id/revoke",
}

// requireMissingKey401 asserts the literal auth-middleware rejection wire
// shape: 401 with success=false and error.code=MISSING_API_KEY, decoded
// from the raw body rather than any production struct.
func requireMissingKey401(t *testing.T, w *httptest.ResponseRecorder, route string) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, w.Code,
		"%s without an API key must be rejected by the auth middleware", route)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "route %s", route)
	assert.Equal(t, false, body["success"], "route %s", route)
	errObj, ok := body["error"].(map[string]interface{})
	require.True(t, ok, "route %s: 401 body must carry an error object", route)
	assert.Equal(t, "MISSING_API_KEY", errObj["code"], "route %s", route)
}

// NOTE: the SET-001 / SET-005 spec citations live in
// backend/cmd/crm-api/oauth_routes_boundary_test.go, which drives the
// PRODUCTION wiring (the real wire chain + registerRoutes) rather than this
// file's test-local reconstruction. The tests below stay as uncited
// registrar-level groundwork: they pin the registrars' own behavior.
func TestOAuthRouteRegistration_AuthBoundary(t *testing.T) {
	router := newOAuthTestRouter(true, true)

	t.Run("callback routes are reachable without the API key", func(t *testing.T) {
		// A keyless request to each provider callback must get PAST the
		// auth middleware and into the handler. With no query params the
		// handler deterministically 302-redirects to the settings surface
		// with an invalid_state outcome — proof it executed.
		for provider, path := range map[string]string{
			"google":  "/api/v1/auth/google/callback",
			"todoist": "/api/v1/auth/todoist/callback",
		} {
			w := serveOAuthTestRequest(router, http.MethodGet, path, false)
			assert.NotEqual(t, http.StatusUnauthorized, w.Code,
				"%s callback must not be rejected by the API-key middleware", provider)
			require.Equal(t, http.StatusFound, w.Code,
				"%s callback handler must run and redirect", provider)
			location := w.Header().Get("Location")
			assert.Contains(t, location, "/settings?auth=error", "provider %s", provider)
			assert.Contains(t, location, "provider="+provider)
		}
	})

	t.Run("auth-URL and account routes without a key are rejected 401", func(t *testing.T) {
		protected := []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/v1/auth/google"},
			{http.MethodGet, "/api/v1/auth/google/accounts"},
			{http.MethodGet, fmt.Sprintf("/api/v1/auth/google/accounts/%s/status", uuid.New())},
			{http.MethodPost, fmt.Sprintf("/api/v1/auth/google/accounts/%s/revoke", uuid.New())},
			{http.MethodGet, "/api/v1/auth/todoist"},
			{http.MethodGet, "/api/v1/auth/todoist/accounts"},
			{http.MethodGet, fmt.Sprintf("/api/v1/auth/todoist/accounts/%s/status", uuid.New())},
			{http.MethodPost, fmt.Sprintf("/api/v1/auth/todoist/accounts/%s/revoke", uuid.New())},
		}
		for _, route := range protected {
			w := serveOAuthTestRequest(router, route.method, route.path, false)
			requireMissingKey401(t, w, route.method+" "+route.path)
		}
	})

	t.Run("auth-URL routes with the key reach their handlers", func(t *testing.T) {
		// Acceptance side of the boundary: the same protected routes
		// proceed once the key is supplied.
		wGoogle := serveOAuthTestRequest(router, http.MethodGet, "/api/v1/auth/google", true)
		require.Equal(t, http.StatusOK, wGoogle.Code)
		assert.Contains(t, wGoogle.Body.String(), "accounts.google.com")

		wTodoist := serveOAuthTestRequest(router, http.MethodGet, "/api/v1/auth/todoist", true)
		require.Equal(t, http.StatusOK, wTodoist.Code)
		assert.Contains(t, wTodoist.Body.String(), "todoist.com/oauth/authorize")
	})
}

func TestOAuthRouteRegistration_ProviderGating(t *testing.T) {
	t.Run("both providers enabled registers both full route sets", func(t *testing.T) {
		router := newOAuthTestRouter(true, true)
		assert.ElementsMatch(t,
			append(append([]string{}, googleOAuthRouteSet...), todoistOAuthRouteSet...),
			oauthRouteSet(router))
	})

	t.Run("google-only registers google routes and no todoist routes", func(t *testing.T) {
		router := newOAuthTestRouter(true, false)
		assert.ElementsMatch(t, googleOAuthRouteSet, oauthRouteSet(router))

		w := serveOAuthTestRequest(router, http.MethodGet, "/api/v1/auth/todoist/callback", false)
		assert.Equal(t, http.StatusNotFound, w.Code,
			"an unconfigured provider's callback URL must return 404")
	})

	t.Run("todoist-only registers todoist routes and no google routes", func(t *testing.T) {
		router := newOAuthTestRouter(false, true)
		assert.ElementsMatch(t, todoistOAuthRouteSet, oauthRouteSet(router))

		w := serveOAuthTestRequest(router, http.MethodGet, "/api/v1/auth/google/callback", false)
		assert.Equal(t, http.StatusNotFound, w.Code,
			"an unconfigured provider's callback URL must return 404")
	})

	t.Run("both providers disabled registers no oauth routes", func(t *testing.T) {
		router := newOAuthTestRouter(false, false)
		assert.Empty(t, oauthRouteSet(router))

		for _, path := range []string{"/api/v1/auth/google/callback", "/api/v1/auth/todoist/callback"} {
			w := serveOAuthTestRequest(router, http.MethodGet, path, false)
			assert.Equal(t, http.StatusNotFound, w.Code,
				"disabled provider callback %s must 404", path)
		}
	})
}
