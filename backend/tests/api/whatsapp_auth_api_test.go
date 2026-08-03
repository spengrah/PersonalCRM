//go:build integration_testdb

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWhatsAppManager drives the endpoint contract without a whatsmeow client.
// Pairing genuinely cannot be completed end to end in a test — no test can scan
// a QR code with a real phone — so the HTTP layer is covered through the seam
// and the manager's own state machine is covered by its unit tests.
type fakeWhatsAppManager struct {
	mu sync.Mutex

	status       wapkg.Status
	startErr     error
	disconnect   *wapkg.DisconnectResult
	disconnErr   error
	cancelCalls  int
	startCalls   int
	lastPairReq  wapkg.PairRequest
	lastForceArg bool
}

func (f *fakeWhatsAppManager) Status() wapkg.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeWhatsAppManager) StartPairing(_ context.Context, req wapkg.PairRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.lastPairReq = req
	return f.startErr
}

func (f *fakeWhatsAppManager) CancelPairing() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
}

func (f *fakeWhatsAppManager) Disconnect(_ context.Context, force bool) (*wapkg.DisconnectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastForceArg = force
	return f.disconnect, f.disconnErr
}

var _ handlers.WhatsAppManager = (*fakeWhatsAppManager)(nil)

// setupWhatsAppRouter builds the production route surface over a fake manager.
func setupWhatsAppRouter(t *testing.T, manager handlers.WhatsAppManager) *gin.Engine {
	t.Helper()
	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterWhatsAppRoutes(v1, handlers.NewWhatsAppHandler(manager))
	return router
}

func doWhatsAppRequest(t *testing.T, router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decodeWhatsAppStatus pulls the status payload out of the API envelope.
func decodeWhatsAppStatus(t *testing.T, rec *httptest.ResponseRecorder) handlers.WhatsAppStatusResponse {
	t.Helper()
	var envelope struct {
		Data handlers.WhatsAppStatusResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope.Data
}

func decodeWhatsAppError(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details"`
} {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details string `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope.Error
}

// --- WHA-006: pairing endpoints ---------------------------------------------

func TestAPI_WhatsAppQRStartReturnsCode(t *testing.T) {
	// spec: WHA-006.qr-start-returns-code
	expires := time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC)
	code := "QR-CODE-1"
	manager := &fakeWhatsAppManager{status: wapkg.Status{
		Configured: true,
		State:      wapkg.StatePairing,
		Pairing:    &wapkg.Pairing{Method: wapkg.PairMethodQR, QRCode: &code, ExpiresAt: expires},
	}}
	router := setupWhatsAppRouter(t, manager)

	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start", handlers.WhatsAppPairRequest{Method: "qr"})

	require.Equal(t, http.StatusAccepted, rec.Code)
	status := decodeWhatsAppStatus(t, rec)
	require.NotNil(t, status.Pairing)
	require.NotNil(t, status.Pairing.QRCode)
	assert.Equal(t, code, *status.Pairing.QRCode)
	assert.Equal(t, wapkg.PairMethodQR, manager.lastPairReq.Method)
}

func TestAPI_WhatsAppQRStartTimesOutWithoutCode(t *testing.T) {
	// spec: WHA-006.qr-start-times-out-without-code
	manager := &fakeWhatsAppManager{startErr: wapkg.ErrQRCodeTimeout}
	router := setupWhatsAppRouter(t, manager)

	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start", handlers.WhatsAppPairRequest{Method: "qr"})

	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Equal(t, "qr_code_timeout", decodeWhatsAppError(t, rec).Details)
}

func TestAPI_WhatsAppStartRefusedUntilIngestWired(t *testing.T) {
	// spec: WHA-006.start-refused-until-ingest-wired
	manager := &fakeWhatsAppManager{startErr: wapkg.ErrIngestNotWired}
	router := setupWhatsAppRouter(t, manager)

	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start", handlers.WhatsAppPairRequest{Method: "qr"})

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, wapkg.ReasonIngestNotWired, decodeWhatsAppError(t, rec).Details,
		"the refusal names the missing piece so the settings page can explain it")
}

func TestAPI_WhatsAppPhoneStartRequiresE164(t *testing.T) {
	// spec: WHA-006.phone-start-requires-e164
	manager := &fakeWhatsAppManager{}
	router := setupWhatsAppRouter(t, manager)

	for _, phone := range []string{"", "5551234567", "+123", "+1-555-123-4567"} {
		rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start",
			handlers.WhatsAppPairRequest{Method: "phone", Phone: phone})
		require.Equal(t, http.StatusBadRequest, rec.Code, "phone %q must be rejected", phone)
	}
	assert.Equal(t, 0, manager.startCalls, "a malformed number never reaches the manager")

	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start",
		handlers.WhatsAppPairRequest{Method: "phone", Phone: "+15551234567"})
	assert.Equal(t, http.StatusAccepted, rec.Code, "a well-formed E.164 number is accepted")
}

func TestAPI_WhatsAppStartRejectsUnknownMethod(t *testing.T) {
	// spec: WHA-006.phone-start-requires-e164
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{})
	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start",
		map[string]string{"method": "carrier-pigeon"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPI_WhatsAppStartWhenConnectedConflicts(t *testing.T) {
	// spec: WHA-006.start-when-connected-conflicts
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{startErr: wapkg.ErrAlreadyConnected})
	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start", handlers.WhatsAppPairRequest{Method: "qr"})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestAPI_WhatsAppStartWhenPairingConflicts(t *testing.T) {
	// spec: WHA-006.start-when-pairing-conflicts
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{startErr: wapkg.ErrPairingInProgress})
	rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/start", handlers.WhatsAppPairRequest{Method: "qr"})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestAPI_WhatsAppCancelIsIdempotent(t *testing.T) {
	// spec: WHA-006.cancel-is-idempotent
	manager := &fakeWhatsAppManager{}
	router := setupWhatsAppRouter(t, manager)

	for i := 0; i < 2; i++ {
		rec := doWhatsAppRequest(t, router, http.MethodPost, "/api/v1/whatsapp/auth/cancel", nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
	}
	assert.Equal(t, 2, manager.cancelCalls)
}

func TestAPI_WhatsAppDisconnectWhenNotPairedConflicts(t *testing.T) {
	// spec: WHA-006.disconnect-when-not-paired-conflicts
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{disconnErr: wapkg.ErrNotPaired})
	rec := doWhatsAppRequest(t, router, http.MethodDelete, "/api/v1/whatsapp/auth", nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestAPI_WhatsAppDisconnectReportsFailureWhenRemoteLogoutFails(t *testing.T) {
	// spec: WHA-006.disconnect-reports-failure-when-remote-logout-fails
	manager := &fakeWhatsAppManager{
		disconnErr: errors.Join(wapkg.ErrRemoteUnlinkFailed, errors.New("server rejected logout")),
		status: wapkg.Status{
			Configured: true,
			State:      wapkg.StateDisconnectFailed,
			Reason:     "server rejected logout",
		},
	}
	router := setupWhatsAppRouter(t, manager)

	rec := doWhatsAppRequest(t, router, http.MethodDelete, "/api/v1/whatsapp/auth", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code,
		"a failed remote unlink must not read as success — the device is still linked")

	statusRec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/auth/status", nil)
	require.Equal(t, http.StatusOK, statusRec.Code)
	assert.Equal(t, wapkg.StateDisconnectFailed, decodeWhatsAppStatus(t, statusRec).State,
		"the state tells the user the credentials were KEPT and a retry is possible")
}

func TestAPI_WhatsAppForceDisconnectClearsLocalStateWithWarning(t *testing.T) {
	// spec: WHA-006.force-disconnect-clears-local-state-with-warning
	manager := &fakeWhatsAppManager{disconnect: &wapkg.DisconnectResult{
		Forced:  true,
		Warning: "Unlink this device from your phone's Linked Devices screen to complete the disconnect.",
	}}
	router := setupWhatsAppRouter(t, manager)

	rec := doWhatsAppRequest(t, router, http.MethodDelete, "/api/v1/whatsapp/auth?force=true", nil)

	require.Equal(t, http.StatusOK, rec.Code, "a 204 could not carry the warning this path promises")
	var envelope struct {
		Data handlers.WhatsAppDisconnectResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	assert.True(t, envelope.Data.Forced)
	assert.Contains(t, envelope.Data.Warning, "Linked Devices")
	assert.True(t, manager.lastForceArg, "?force=true must reach the manager")
}

func TestAPI_WhatsAppDisconnectSucceedsWithBody(t *testing.T) {
	// spec: WHA-006.disconnect-when-not-paired-conflicts
	manager := &fakeWhatsAppManager{disconnect: &wapkg.DisconnectResult{RemoteUnlinked: true}}
	router := setupWhatsAppRouter(t, manager)

	rec := doWhatsAppRequest(t, router, http.MethodDelete, "/api/v1/whatsapp/auth", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data handlers.WhatsAppDisconnectResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	assert.True(t, envelope.Data.RemoteUnlinked)
	assert.False(t, manager.lastForceArg)
}

// --- WHA-007: status endpoint ------------------------------------------------

func TestAPI_WhatsAppStatusReportsState(t *testing.T) {
	// spec: WHA-007.status-reports-state
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{status: wapkg.Status{
		Configured: true,
		State:      wapkg.StateNotPaired,
	}})

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/auth/status", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeWhatsAppStatus(t, rec)
	assert.True(t, status.Configured)
	assert.Equal(t, wapkg.StateNotPaired, status.State)
}

func TestAPI_WhatsAppStatusReportsNotReadyReason(t *testing.T) {
	// spec: WHA-007.status-reports-not-ready-reason
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{status: wapkg.Status{
		Configured: true,
		State:      wapkg.StateNotReady,
		Reason:     wapkg.ReasonIngestNotWired,
	}})

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/auth/status", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeWhatsAppStatus(t, rec)
	assert.Equal(t, wapkg.StateNotReady, status.State)
	assert.Equal(t, wapkg.ReasonIngestNotWired, status.Reason)
}

func TestAPI_WhatsAppStatusReportsAccountIdentity(t *testing.T) {
	// spec: WHA-007.status-reports-account-identity
	jid := "15551234567@s.whatsapp.net"
	phone := "15551234567"
	pushName := "Test Account"
	connectedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{status: wapkg.Status{
		Configured:  true,
		State:       wapkg.StateConnected,
		JID:         &jid,
		PhoneNumber: &phone,
		PushName:    &pushName,
		ConnectedAt: &connectedAt,
	}})

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/auth/status", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeWhatsAppStatus(t, rec)
	assert.Equal(t, wapkg.StateConnected, status.State)
	require.NotNil(t, status.JID)
	assert.Equal(t, jid, *status.JID)
	require.NotNil(t, status.PhoneNumber)
	assert.Equal(t, phone, *status.PhoneNumber)
	require.NotNil(t, status.PushName)
	assert.Equal(t, pushName, *status.PushName)
	require.NotNil(t, status.ConnectedAt)
	assert.Equal(t, connectedAt.Format(time.RFC3339), *status.ConnectedAt)
}

func TestAPI_WhatsAppStatusCarriesLivePairingCode(t *testing.T) {
	// spec: WHA-007.status-carries-live-pairing-code
	pairCode := "ABCD1234"
	expires := time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC)
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{status: wapkg.Status{
		Configured: true,
		State:      wapkg.StatePairing,
		Pairing:    &wapkg.Pairing{Method: wapkg.PairMethodPhone, PairCode: &pairCode, ExpiresAt: expires},
	}})

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/auth/status", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeWhatsAppStatus(t, rec)
	require.NotNil(t, status.Pairing)
	assert.Equal(t, wapkg.PairMethodPhone, status.Pairing.Method)
	require.NotNil(t, status.Pairing.PairCode)
	assert.Equal(t, pairCode, *status.Pairing.PairCode)
	assert.Equal(t, expires.Format(time.RFC3339), status.Pairing.ExpiresAt,
		"the expiry has to reach the client, or a stale code looks live")
}

func TestAPI_WhatsAppStatusAbsentWhenFeatureDisabled(t *testing.T) {
	// spec: WHA-007.status-absent-when-feature-disabled
	// With the feature off no handler is built, so the routes are never
	// registered and gin's own 404 answers — which is exactly what the
	// settings page reads as "configuration required".
	router := gin.New()
	v1 := router.Group("/api/v1")
	_ = v1

	for _, path := range []string{
		"/api/v1/whatsapp/auth/status",
		"/api/v1/whatsapp/auth/start",
		"/api/v1/whatsapp/auth/cancel",
		"/api/v1/whatsapp/auth",
	} {
		rec := doWhatsAppRequest(t, router, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s must not exist when WhatsApp is disabled", path)
	}
}

func TestAPI_WhatsAppStatusReportsBackfillCounts(t *testing.T) {
	// spec: WHA-007.status-reports-backfill-counts
	floor := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	router := setupWhatsAppRouter(t, &fakeWhatsAppManager{status: wapkg.Status{
		Configured: true,
		State:      wapkg.StateConnected,
		Backfill: wapkg.BackfillStatus{
			Pending:             2,
			Processing:          1,
			Failed:              3,
			DroppedInlineChunks: 4,
			ObservedFloorAt:     &floor,
		},
	}})

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/auth/status", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	backfill := decodeWhatsAppStatus(t, rec).Backfill
	assert.Equal(t, 2, backfill.Pending)
	assert.Equal(t, 1, backfill.Processing)
	assert.Equal(t, 3, backfill.Failed)
	assert.Equal(t, 4, backfill.DroppedInlineChunks,
		"a dropped bootstrap chunk is an accepted gap, but it has to be visible")
	require.NotNil(t, backfill.ObservedFloorAt)
	assert.Equal(t, floor.Format(time.RFC3339), *backfill.ObservedFloorAt)
}
