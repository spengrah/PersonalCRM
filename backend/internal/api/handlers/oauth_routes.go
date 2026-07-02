package handlers

import "github.com/gin-gonic/gin"

// OAuthCallbackDeps bundles the OAuth handler and the per-provider gate
// that both the callback routes and the authenticated auth-URL routes
// need. GoogleEnabled mirrors run()'s googleOAuthService != nil check;
// the Todoist gate is read from Handler.HasTodoistOAuth() so the two
// providers register independently, matching today's per-route gating.
type OAuthCallbackDeps struct {
	Handler       *OAuthHandler
	GoogleEnabled bool
}

// RegisterOAuthCallbackRoutes wires the provider-redirect callback routes
// directly onto the bare router (NO auth — external providers cannot
// carry the global API key):
//
//   - GET /api/v1/auth/google/callback  (only if GoogleEnabled)
//   - GET /api/v1/auth/todoist/callback (only if Handler.HasTodoistOAuth())
//
// Caller gates the whole call on oauthHandler != nil.
func RegisterOAuthCallbackRoutes(router *gin.Engine, deps OAuthCallbackDeps) {
	if deps.GoogleEnabled {
		router.GET("/api/v1/auth/google/callback", deps.Handler.GoogleCallback)
	}
	if deps.Handler.HasTodoistOAuth() {
		router.GET("/api/v1/auth/todoist/callback", deps.Handler.TodoistCallback)
	}
}

// RegisterOAuthRoutes wires the authenticated OAuth auth-URL + account
// management route surface onto a group whose middleware already
// enforces the global API key:
//
//   - GET  /api/v1/auth/google                        (only if GoogleEnabled)
//   - GET  /api/v1/auth/google/accounts               (only if GoogleEnabled)
//   - GET  /api/v1/auth/google/accounts/:id/status    (only if GoogleEnabled)
//   - POST /api/v1/auth/google/accounts/:id/revoke    (only if GoogleEnabled)
//   - GET  /api/v1/auth/todoist                       (only if Handler.HasTodoistOAuth())
//   - GET  /api/v1/auth/todoist/accounts              (only if Handler.HasTodoistOAuth())
//   - GET  /api/v1/auth/todoist/accounts/:id/status   (only if Handler.HasTodoistOAuth())
//   - POST /api/v1/auth/todoist/accounts/:id/revoke   (only if Handler.HasTodoistOAuth())
//
// Caller gates the whole call on oauthHandler != nil.
func RegisterOAuthRoutes(v1 *gin.RouterGroup, deps OAuthCallbackDeps) {
	authRoutes := v1.Group("/auth")
	{
		// Google OAuth (only if configured)
		if deps.GoogleEnabled {
			authRoutes.GET("/google", deps.Handler.GetGoogleAuthURL)
			authRoutes.GET("/google/accounts", deps.Handler.ListGoogleAccounts)
			authRoutes.GET("/google/accounts/:id/status", deps.Handler.GetGoogleAccountStatus)
			authRoutes.POST("/google/accounts/:id/revoke", deps.Handler.RevokeGoogleAccount)
		}

		// Todoist OAuth (only if configured)
		if deps.Handler.HasTodoistOAuth() {
			authRoutes.GET("/todoist", deps.Handler.GetTodoistAuthURL)
			authRoutes.GET("/todoist/accounts", deps.Handler.ListTodoistAccounts)
			authRoutes.GET("/todoist/accounts/:id/status", deps.Handler.GetTodoistAccountStatus)
			authRoutes.POST("/todoist/accounts/:id/revoke", deps.Handler.RevokeTodoistAccount)
		}
	}
}
