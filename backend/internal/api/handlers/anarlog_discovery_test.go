package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newDiscoveryRouter(t *testing.T, h *AnarlogDiscoveryHandler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/imports/anarlog-title", h.ListAnarlogTitle)
	router.POST("/api/v1/imports/anarlog-title/resolve", h.ResolveAnarlogTitle)
	return router
}

func postJSON(t *testing.T, router *gin.Engine, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var buf []byte
	switch b := body.(type) {
	case string:
		buf = []byte(b)
	case nil:
		buf = nil
	default:
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		buf = raw
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

// TestResolveAnarlogTitle_EmptyBody — missing required normalized_token
// + action yields 400 before any service call.
func TestResolveAnarlogTitle_EmptyBody(t *testing.T) {
	h := NewAnarlogDiscoveryHandler(nil)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve", "{}")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveAnarlogTitle_UnknownAction — action outside the oneof set
// is rejected with 400.
func TestResolveAnarlogTitle_UnknownAction(t *testing.T) {
	h := NewAnarlogDiscoveryHandler(nil)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "lena", "action": "delete"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveAnarlogTitle_InvalidCadence — a bad cadence value is
// rejected by the oneof validator.
func TestResolveAnarlogTitle_InvalidCadence(t *testing.T) {
	h := NewAnarlogDiscoveryHandler(nil)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "lena", "action": "import", "cadence": "hourly"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveAnarlogTitle_LinkMissingContactID — link without
// crm_contact_id is a 400.
func TestResolveAnarlogTitle_LinkMissingContactID(t *testing.T) {
	h := NewAnarlogDiscoveryHandler(nil)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "lena", "action": "link"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "crm_contact_id")
}

// TestResolveAnarlogTitle_LinkInvalidContactID — link with a non-UUID
// crm_contact_id is a 400.
func TestResolveAnarlogTitle_LinkInvalidContactID(t *testing.T) {
	h := NewAnarlogDiscoveryHandler(nil)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "lena", "action": "link", "crm_contact_id": "not-a-uuid"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// --- service-backed sentinel mapping ---

// fakeDiscoveryRepo lets the handler tests drive the service through to
// its sentinel branches without a database.
type fakeDiscoveryRepo struct {
	groups   []repository.AnarlogTitleGroup
	siblings []repository.ExternalContact
}

func (f *fakeDiscoveryRepo) ListAnarlogTitleGroups(ctx context.Context) ([]repository.AnarlogTitleGroup, error) {
	return f.groups, nil
}
func (f *fakeDiscoveryRepo) FindAnarlogTitleSiblingsByToken(ctx context.Context, token string) ([]repository.ExternalContact, error) {
	return f.siblings, nil
}
func (f *fakeDiscoveryRepo) MarkAnarlogTitleSiblingsImportedByToken(ctx context.Context, token string, id uuid.UUID) (int64, error) {
	return 1, nil
}
func (f *fakeDiscoveryRepo) MarkAnarlogTitleSiblingsMatchedByToken(ctx context.Context, token string, id uuid.UUID) (int64, error) {
	return 1, nil
}
func (f *fakeDiscoveryRepo) MarkAnarlogTitleSiblingsIgnoredByToken(ctx context.Context, token string) error {
	return nil
}

// fakeDiscoveryContacts is a no-op contact writer; the handler sentinel
// tests for missing token groups never reach it.
type fakeDiscoveryContacts struct{}

func (f *fakeDiscoveryContacts) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	return &repository.Contact{ID: id, FullName: "Stub"}, nil
}
func (f *fakeDiscoveryContacts) CreateContact(ctx context.Context, req repository.CreateContactRequest, methods []service.ContactMethodInput) (*repository.Contact, uuid.UUID, error) {
	return &repository.Contact{ID: uuid.New(), FullName: req.FullName}, uuid.Nil, nil
}
func (f *fakeDiscoveryContacts) UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest, methods []service.ContactMethodInput, replaceMethods bool) (*repository.Contact, uuid.UUID, error) {
	return &repository.Contact{ID: id, FullName: req.FullName}, uuid.Nil, nil
}
func (f *fakeDiscoveryContacts) DeleteContact(ctx context.Context, id uuid.UUID) error {
	return nil
}

// TestResolveAnarlogTitle_UnknownToken — an empty sibling set maps to 404.
func TestResolveAnarlogTitle_UnknownToken(t *testing.T) {
	svc := service.NewAnarlogDiscoveryService(&fakeDiscoveryRepo{siblings: nil}, &fakeDiscoveryContacts{})
	h := NewAnarlogDiscoveryHandler(svc)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "ghost", "action": "ignore"})
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestResolveAnarlogTitle_IgnoreSuccess — ignore with live siblings
// returns 200 and omits contact_id.
func TestResolveAnarlogTitle_IgnoreSuccess(t *testing.T) {
	svc := service.NewAnarlogDiscoveryService(
		&fakeDiscoveryRepo{siblings: []repository.ExternalContact{{ID: uuid.New(), Source: "anarlog_title"}}},
		&fakeDiscoveryContacts{},
	)
	h := NewAnarlogDiscoveryHandler(svc)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "lena", "action": "ignore"})
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "contact_id")
}

// TestResolveAnarlogTitle_ImportReturnsContactID — import returns 200
// with a contact_id in the body.
func TestResolveAnarlogTitle_ImportReturnsContactID(t *testing.T) {
	dn := "Lena"
	svc := service.NewAnarlogDiscoveryService(
		&fakeDiscoveryRepo{siblings: []repository.ExternalContact{{ID: uuid.New(), Source: "anarlog_title", DisplayName: &dn}}},
		&fakeDiscoveryContacts{},
	)
	h := NewAnarlogDiscoveryHandler(svc)
	router := newDiscoveryRouter(t, h)
	w := postJSON(t, router, "/api/v1/imports/anarlog-title/resolve",
		map[string]any{"normalized_token": "lena", "action": "import"})
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "contact_id")
}

// TestListAnarlogTitle_NilService — the list endpoint returns 503 when
// the service is not wired.
func TestListAnarlogTitle_NilService(t *testing.T) {
	h := NewAnarlogDiscoveryHandler(nil)
	router := newDiscoveryRouter(t, h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/imports/anarlog-title", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestAnarlogTitleRoute_DoesNotShadowGetCandidate proves Gin's routing
// tree resolves the static /imports/anarlog-title segment to the
// discovery handler while /imports/<uuid> still resolves to the :id
// candidate handler, when the static routes are declared before :id
// exactly as cmd/crm-api/main.go registers them. Uses sentinel handlers
// so the test isolates routing behavior from handler internals.
func TestAnarlogTitleRoute_DoesNotShadowGetCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	imports := router.Group("/api/v1/imports")
	{
		imports.GET("/candidates", func(c *gin.Context) { c.String(http.StatusOK, "candidates") })
		imports.GET("/anarlog-title", func(c *gin.Context) { c.String(http.StatusOK, "discovery") })
		imports.POST("/anarlog-title/resolve", func(c *gin.Context) { c.String(http.StatusOK, "resolve") })
		imports.GET("/:id", func(c *gin.Context) { c.String(http.StatusOK, "candidate:"+c.Param("id")) })
	}

	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/api/v1/imports/anarlog-title", "discovery"},
		{http.MethodPost, "/api/v1/imports/anarlog-title/resolve", "resolve"},
		{http.MethodGet, "/api/v1/imports/" + uuid.New().String(), "candidate:"},
		{http.MethodGet, "/api/v1/imports/candidates", "candidates"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, tc.path)
		require.Contains(t, w.Body.String(), tc.want, tc.path)
	}
}
