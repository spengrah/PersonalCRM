package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newSuggestionRoutesRouter registers the imports route group in the SAME
// order as main.go: the static `suggestions` routes BEFORE the `/:id`
// wildcard. This is the regression guard for the Gin shadowing gotcha —
// if `suggestions` were registered after `/:id`, the tree build would
// either panic or route `/imports/suggestions` into the :id handler.
func newSuggestionRoutesRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()

	idHandler := func(c *gin.Context) {
		c.String(http.StatusOK, "id:"+c.Param("id"))
	}
	listSuggestions := func(c *gin.Context) { c.String(http.StatusOK, "suggestions-list") }
	resolve := func(c *gin.Context) { c.String(http.StatusOK, "resolve:"+c.Param("id")) }
	dismiss := func(c *gin.Context) { c.String(http.StatusOK, "dismiss:"+c.Param("id")) }

	imports := router.Group("/api/v1/imports")
	{
		imports.GET("/candidates", func(c *gin.Context) { c.String(http.StatusOK, "candidates") })
		imports.GET("/suggestions", listSuggestions)
		imports.POST("/suggestions/:id/methods/resolve", resolve)
		imports.POST("/suggestions/:id/methods/dismiss", dismiss)
		imports.GET("/:id", idHandler)
		imports.POST("/:id/import", func(c *gin.Context) { c.String(http.StatusOK, "import:"+c.Param("id")) })
	}
	return router
}

func TestSuggestionRoutes_StaticBeforeWildcard(t *testing.T) {
	router := newSuggestionRoutesRouter(t)

	// GET /imports/suggestions must hit the list handler, NOT the :id
	// handler that would 200 with "id:suggestions".
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/imports/suggestions", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "suggestions-list", w.Body.String())

	// GET /imports/<uuid> still resolves to the :id handler.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/imports/abc-123", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "id:abc-123", w.Body.String())

	// POST /imports/suggestions/<uuid>/methods/resolve hits the resolve
	// handler with the id param bound at the suggestions depth.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/imports/suggestions/xyz-789/methods/resolve", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "resolve:xyz-789", w.Body.String())
}
