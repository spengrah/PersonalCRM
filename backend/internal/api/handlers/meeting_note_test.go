package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubMeetingNoteService records the most recent ResolveLink invocation
// for assertion. Implements the methods of *service.MeetingNoteService
// the handler calls (only ResolveLink + ListNeedsAttention) — but since
// the handler holds the concrete struct, we set it to nil and use the
// 503 short-circuit to verify the body-validation paths run first.
type stubMeetingNoteService struct{}

func newTestHandler(t *testing.T, svc *service.MeetingNoteService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewMeetingNoteHandler(svc)
	router := gin.New()
	router.GET("/api/v1/meeting-notes/needs-attention", h.ListNeedsAttention)
	router.POST("/api/v1/meeting-notes/:id/resolve-link", h.ResolveLink)
	return router
}

// post is a small helper that posts a JSON body and returns the
// response recorder + decoded JSON body.
func post(t *testing.T, router *gin.Engine, path string, body interface{}) *httptest.ResponseRecorder {
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

// TestResolveLink_PathParamInvalidUUID — handler returns 400 when the
// :id path param is not a UUID, regardless of body shape.
func TestResolveLink_PathParamInvalidUUID(t *testing.T) {
	router := newTestHandler(t, nil)
	w := post(t, router, "/api/v1/meeting-notes/not-a-uuid/resolve-link",
		map[string]interface{}{"action": "none_of_these"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "Invalid meeting_note ID")
}

// TestResolveLink_EmptyBody — handler returns 400 when the body is
// empty (action is required).
// spec: NTS-018.action-required-must-link
func TestResolveLink_EmptyBody(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link", "{}")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveLink_NullBody — handler returns 400 on literal null JSON.
// spec: NTS-018.action-required-must-link
func TestResolveLink_NullBody(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link", "null")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveLink_UnknownAction — handler returns 400 for an action
// outside {link, none_of_these}.
// spec: NTS-018.action-required-must-link
func TestResolveLink_UnknownAction(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "delete"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveLink_LinkMissingKindAndID — action=link without kind+id
// returns 400.
// spec: NTS-018.action-link-requires-kind
func TestResolveLink_LinkMissingKindAndID(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "link"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "requires kind and id")
}

// TestResolveLink_LinkMissingID — action=link with only kind returns 400.
// spec: NTS-018.action-link-requires-kind
func TestResolveLink_LinkMissingID(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "link", "kind": "event"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveLink_LinkMissingKind — action=link with only id returns 400.
// spec: NTS-018.action-link-requires-kind
func TestResolveLink_LinkMissingKind(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "link", "id": uuid.New().String()})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveLink_LinkInvalidKind — action=link with an unrecognized
// kind returns 400.
// spec: NTS-018.action-link-requires-kind
func TestResolveLink_LinkInvalidKind(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "link", "kind": "hangout", "id": uuid.New().String()})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "kind in {event, phone_call}")
}

// TestResolveLink_LinkInvalidIDFormat — action=link with a malformed
// uuid returns 400.
// spec: NTS-018.action-link-requires-kind
func TestResolveLink_LinkInvalidIDFormat(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "link", "kind": "event", "id": "not-a-uuid"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "id as UUID")
}

// TestResolveLink_NoneOfTheseToleratesExtraFields — defense in depth:
// presence of kind+id with action=none_of_these does not break the
// body parse. The handler dispatches as "none of these" regardless.
// We confirm the validation gate accepts the body shape; downstream
// 503 from the nil svc is OK (we're testing validation only).
func TestResolveLink_NoneOfTheseToleratesExtraFields(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "none_of_these", "kind": "event", "id": uuid.New().String()})
	// nil svc → 503; the body parse succeeded.
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestResolveLink_NoneOfTheseValidBody — confirms a minimal
// none_of_these body is accepted (validation passes; downstream 503
// indicates svc dispatch was attempted).
func TestResolveLink_NoneOfTheseValidBody(t *testing.T) {
	router := newTestHandler(t, nil)
	id := uuid.New().String()
	w := post(t, router, "/api/v1/meeting-notes/"+id+"/resolve-link",
		map[string]interface{}{"action": "none_of_these"})
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestListNeedsAttention_InvalidHostID — bad host_id query param
// returns 400.
// spec: NTS-025.malformed-host-id-query
func TestListNeedsAttention_InvalidHostID(t *testing.T) {
	router := newTestHandler(t, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/meeting-notes/needs-attention?host_id=not-uuid", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestMapResolveError_DomainNotFound — a domain not-found sentinel still
// maps to 404 after the default arm was routed through RespondInternal.
func TestMapResolveError_DomainNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMeetingNoteHandler(nil) // mapResolveError does not touch svc
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	h.mapResolveError(c, service.ErrResolveLinkRowNotFound)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "Meeting note not found")
	// A not-found is expected, not a server fault: c.Errors stays empty.
	require.Empty(t, c.Errors)
}

// TestMapResolveError_GenericNoLeak — an unmapped error hits the default
// arm: it must 500 with a generic body (no err.Error() leak) and append
// the cause to c.Errors so the access log carries it.
func TestMapResolveError_GenericNoLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMeetingNoteHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	h.mapResolveError(c, errors.New("secret internal detail"))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "secret internal detail")
	require.Contains(t, w.Body.String(), `"success":false`)
	require.Len(t, c.Errors, 1)
}

// Ensure stubMeetingNoteService stays a placeholder — used only to
// keep the compiler happy if future tests want to substitute a fake.
var _ = (*stubMeetingNoteService)(nil)

// TestNewMeetingNoteResponse — covers the response projection.
func TestNewMeetingNoteResponse(t *testing.T) {
	id := uuid.New()
	sessID := uuid.New()
	kind := "event"
	linkedID := uuid.New()
	hostID := uuid.New()
	title := "synthetic"
	row := &repository.MeetingNote{
		ID:               id,
		AnarlogSessionID: sessID,
		Title:            &title,
		LinkageState:     repository.LinkageStateLinked,
		LinkedKind:       &kind,
		LinkedID:         &linkedID,
		MacHostID:        &hostID,
	}
	got := newMeetingNoteResponse(row)
	require.NotNil(t, got)
	require.Equal(t, id, got.ID)
	require.Equal(t, sessID, got.AnarlogSessionID)
	require.Equal(t, "synthetic", *got.Title)
	require.Equal(t, "event", *got.LinkedKind)
	require.Equal(t, linkedID, *got.LinkedID)
	require.Equal(t, hostID, *got.MacHostID)
	require.Nil(t, newMeetingNoteResponse(nil))
}
