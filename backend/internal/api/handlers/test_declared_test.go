package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-crm/backend/internal/api"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cover the request-validation paths that must reject BEFORE any DB work
// happens, so the handler is constructed with a nil database on purpose: a case
// that reached the declared-seeding path would fail loudly here rather than
// pass by touching a database it should never have needed.
func newDeclaredTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewTestHandler(nil, nil, nil)
	router := gin.New()
	v1 := router.Group("/api/v1")
	RegisterTestRoutes(v1, handler)
	return router
}

func postDeclaredJSON(t *testing.T, router *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) api.APIResponse {
	t.Helper()
	var resp api.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestSeedDeclaredRejectsMalformedRequests(t *testing.T) {
	router := newDeclaredTestRouter()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing behavior_id", map[string]any{"namespace": "ns-a"}},
		{"missing namespace", map[string]any{"behavior_id": "CAD-026"}},
		{"empty namespace", map[string]any{"behavior_id": "CAD-026", "namespace": ""}},
		{"oversize namespace", map[string]any{"behavior_id": "CAD-026", "namespace": strings.Repeat("a", 61)}},
		// Inside the 60-character TOKEN grammar, but with no room left for the
		// -sN suffix construction may append. Accepting it would seed a world
		// whose effective namespace cleanup then refuses to accept back.
		{"no room for the re-salt suffix", map[string]any{"behavior_id": "CAD-026", "namespace": strings.Repeat("a", 58)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postDeclaredJSON(t, router, "/api/v1/test/seed/declared", tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			resp := decodeEnvelope(t, w)
			require.NotNil(t, resp.Error)
			assert.Equal(t, api.ErrCodeValidation, resp.Error.Code)
		})
	}
}

func TestSeedDeclaredRejectsUnseedableBehaviors(t *testing.T) {
	router := newDeclaredTestRouter()

	cases := map[string]string{
		// Never registered at all.
		"unknown behavior": "ZZZ-999",
		// Registered as needing NO fixture: asking to seed one is a client bug,
		// not an empty success.
		"no-fixture behavior": "DSH-002",
	}
	for name, behaviorID := range cases {
		t.Run(name, func(t *testing.T) {
			w := postDeclaredJSON(t, router, "/api/v1/test/seed/declared", map[string]any{
				"behavior_id": behaviorID, "namespace": "ns-validation",
			})
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, api.ErrCodeValidation, decodeEnvelope(t, w).Error.Code)
		})
	}
}

// The -sN suffix is reserved for internal re-salting; reserving it is what makes
// cleanup's requested → salted-variant expansion unambiguous.
func TestSeedDeclaredRejectsReservedSaltSuffix(t *testing.T) {
	router := newDeclaredTestRouter()
	w := postDeclaredJSON(t, router, "/api/v1/test/seed/declared", map[string]any{
		"behavior_id": "CAD-026", "namespace": "w3-1700-c1-s1",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, decodeEnvelope(t, w).Error.Details, "reserved")
}

func TestSeedDeclaredRejectsInvalidNamespaceCharset(t *testing.T) {
	router := newDeclaredTestRouter()
	for _, ns := range []string{"UPPER", "under_score", "pct%sign"} {
		w := postDeclaredJSON(t, router, "/api/v1/test/seed/declared", map[string]any{
			"behavior_id": "CAD-026", "namespace": ns,
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, "namespace %q", ns)
	}
}

func TestCleanupRequiresExactlyOneShape(t *testing.T) {
	router := newDeclaredTestRouter()

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"neither", map[string]any{}, "neither was provided"},
		{"both", map[string]any{"prefix": "w1-123", "namespaces": []string{"w1-123-c1"}}, "both were provided"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postDeclaredJSON(t, router, "/api/v1/test/cleanup", tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, tc.want, decodeEnvelope(t, w).Error.Details)
		})
	}
}

func TestCleanupRejectsOversizeNamespaceLists(t *testing.T) {
	router := newDeclaredTestRouter()
	tooMany := make([]string, 33)
	for i := range tooMany {
		tooMany[i] = "ns-" + strings.Repeat("a", i+1)
	}
	w := postDeclaredJSON(t, router, "/api/v1/test/cleanup", map[string]any{"namespaces": tooMany})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, api.ErrCodeValidation, decodeEnvelope(t, w).Error.Code)
}

func TestCleanupRejectsMalformedNamespaceEntries(t *testing.T) {
	router := newDeclaredTestRouter()
	for _, ns := range []string{"", strings.Repeat("a", 61)} {
		w := postDeclaredJSON(t, router, "/api/v1/test/cleanup", map[string]any{"namespaces": []string{ns}})
		assert.Equal(t, http.StatusBadRequest, w.Code, "namespace %q", ns)
	}
}
