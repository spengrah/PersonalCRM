//go:build integration_testdb

// WhatsApp route-gating pin against the PRODUCTION route tree.
//
// The settings surface recognises an unconfigured WhatsApp integration by a 404
// from GET /whatsapp/auth/status — which is only true if the routes are absent
// when the feature is off. That is a claim about the composition root, not about
// a handler: a test that builds an empty router proves nothing, because it would
// pass just as well if run() always constructed the handler.
//
// This file reuses the oauth_routes_boundary_test.go wire-chain harness, so the
// enumerated tree is exactly what run() would serve for each config shape.
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postWhatsAppJSON issues an authenticated POST carrying a JSON body. The shared
// oauth helper sends no body, which the pairing endpoint rejects at binding
// before the manager is ever consulted.
func postWhatsAppJSON(router *gin.Engine, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", oauthWiringTestAPIKey)
	router.ServeHTTP(w, req)
	return w
}

// whatsAppRouteSet returns "METHOD path" for every registered /api/v1/whatsapp
// route.
func whatsAppRouteSet(router *gin.Engine) []string {
	var out []string
	for _, r := range router.Routes() {
		if strings.HasPrefix(r.Path, "/api/v1/whatsapp") {
			out = append(out, r.Method+" "+r.Path)
		}
	}
	return out
}

var whatsAppRouteSetExpected = []string{
	"POST /api/v1/whatsapp/auth/start",
	"POST /api/v1/whatsapp/auth/cancel",
	"DELETE /api/v1/whatsapp/auth",
	"GET /api/v1/whatsapp/auth/status",
}

func TestWhatsAppRouteWiring_FeatureGating(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Run("disabled registers no whatsapp routes", func(t *testing.T) {
		// spec: WHA-007.status-absent-when-feature-disabled
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		cfg.Features.EnableWhatsAppSync = false
		router := buildRouterForOAuthWiring(t, cfg)

		assert.Empty(t, whatsAppRouteSet(router),
			"with WhatsApp disabled no /api/v1/whatsapp route may exist")

		// The 404 the settings surface reads as "configuration required" comes
		// from gin itself, because the route was never registered.
		w := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/whatsapp/auth/status", true)
		assert.Equal(t, http.StatusNotFound, w.Code)

		// Sanity: the router is a real, populated tree, so the absence above is
		// a real absence rather than an empty capture.
		require.NotEmpty(t, router.Routes())
	})

	t.Run("enabled registers the full whatsapp route set", func(t *testing.T) {
		// spec: WHA-007.status-reports-state
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		cfg.Features.EnableWhatsAppSync = true
		router := buildRouterForOAuthWiring(t, cfg)

		assert.ElementsMatch(t, whatsAppRouteSetExpected, whatsAppRouteSet(router))

		// And the enabled surface answers rather than 404s — the discriminator
		// against a gate that registers nothing in either shape.
		w := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/whatsapp/auth/status", true)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("enabled surface reports not_ready without connecting", func(t *testing.T) {
		// spec: WHA-013
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		cfg.Features.EnableWhatsAppSync = true
		router := buildRouterForOAuthWiring(t, cfg)

		w := serveOAuthWiringRequest(router, http.MethodGet, "/api/v1/whatsapp/auth/status", true)
		require.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, `"state":"not_ready"`,
			"the shipped wiring installs only the history recorder, so the client must refuse to connect")
		assert.Contains(t, body, `"missing":`,
			"the status names the dependency it is waiting on")

		// Pairing is refused on the same precondition, through the real router
		// and the real manager — not a fake that was told to refuse.
		start := postWhatsAppJSON(router, "/api/v1/whatsapp/auth/start", `{"method":"qr"}`)
		assert.Equal(t, http.StatusConflict, start.Code)
		assert.Contains(t, start.Body.String(), "ingest_not_wired")
		assert.Contains(t, start.Body.String(), "message ingestor is not wired",
			"the refusal names the dependency the shipped wiring is missing")
	})
}
