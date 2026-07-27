package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubMacHostService satisfies the handler's MacHostService interface
// with hand-scripted return values. Only RotateAPIKey is exercised by
// this file; the other methods panic on call so a stray invocation is
// surfaced as a test failure rather than masked as success.
type stubMacHostService struct {
	rotateResult *service.RotateAPIKeyResult
	rotateErr    error
	rotateCalls  int

	heartbeatResult *repository.MacHost
	heartbeatErr    error
	heartbeatCalls  int
}

func (s *stubMacHostService) RotateAPIKey(_ context.Context, _ uuid.UUID, _ string, _ string) (*service.RotateAPIKeyResult, error) {
	s.rotateCalls++
	return s.rotateResult, s.rotateErr
}

func (s *stubMacHostService) CreatePairingToken(_ context.Context) (string, time.Time, error) {
	panic("CreatePairingToken not expected in this test")
}
func (s *stubMacHostService) PairWithToken(_ context.Context, _ string, _ string, _ string, _ int32) (*service.PairResult, error) {
	panic("PairWithToken not expected in this test")
}
func (s *stubMacHostService) Heartbeat(_ context.Context, _ uuid.UUID, _ repository.HeartbeatPayload) (*repository.MacHost, error) {
	s.heartbeatCalls++
	if s.heartbeatResult != nil || s.heartbeatErr != nil {
		return s.heartbeatResult, s.heartbeatErr
	}
	panic("Heartbeat not expected in this test")
}
func (s *stubMacHostService) CommitCursor(_ context.Context, _ repository.CommitMacHostCursorParams) error {
	panic("CommitCursor not expected in this test")
}
func (s *stubMacHostService) GetCursor(_ context.Context, _ string, _ uuid.UUID) (*repository.MacHostCursor, error) {
	panic("GetCursor not expected in this test")
}
func (s *stubMacHostService) ListActiveHosts(_ context.Context) ([]*repository.MacHost, error) {
	panic("ListActiveHosts not expected in this test")
}
func (s *stubMacHostService) GetHost(_ context.Context, _ uuid.UUID) (*repository.MacHost, error) {
	panic("GetHost not expected in this test")
}
func (s *stubMacHostService) RevokeHost(_ context.Context, _ uuid.UUID) error {
	panic("RevokeHost not expected in this test")
}
func (s *stubMacHostService) KnownIdentifiers(_ context.Context) (*service.KnownIdentifiersResult, error) {
	panic("KnownIdentifiers not expected in this test")
}
func (s *stubMacHostService) KnownIDsForSource(_ context.Context, _ uuid.UUID, _ string) ([]service.KnownExternalContactID, error) {
	panic("KnownIDsForSource not expected in this test")
}
func (s *stubMacHostService) GetSourceCounts(_ context.Context, _ uuid.UUID) (map[string]int, error) {
	panic("GetSourceCounts not expected in this test")
}

// fakeHostRepo is a deterministic MacHostKeyValidator that returns a
// pre-canned host for the middleware's lookup. Mirrors the auth
// package's own fake (unexported there).
type fakeHostRepo struct {
	host *repository.MacHost
}

func (r *fakeHostRepo) GetActiveHostByID(_ context.Context, id uuid.UUID) (*repository.MacHost, error) {
	if r.host == nil || r.host.ID != id {
		return nil, db.ErrNotFound
	}
	return r.host, nil
}

// alwaysMatch satisfies PasswordComparator by accepting any pair —
// the middleware-vs-handler boundary is what we're testing, not the
// bcrypt path.
func alwaysMatch(_ []byte, _ []byte) error { return nil }

// TestRotateKey_HostRevokedBetweenMiddlewareAndTx covers the handler's
// db.ErrNotFound → 404 branch. The service stub returns ErrNotFound
// (modelling the rare race where the host row was revoked between
// the middleware read and the rotate tx's FOR UPDATE lookup).
// Deterministically exercises the handler's response shape without
// needing tx-internal hooks.
// spec: MAC-010.missing-revoked-host
func TestRotateKey_HostRevokedBetweenMiddlewareAndTx(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hostID := uuid.New()
	stubHost := &repository.MacHost{
		ID:         hostID,
		Hostname:   "test-host",
		APIKeyHash: "test-hash",
	}
	stub := &stubMacHostService{rotateErr: db.ErrNotFound}
	handler := NewMacHostHandler(stub, nil)

	r := gin.New()
	r.Use(auth.MacHostAuthMiddleware(
		&fakeHostRepo{host: stubHost},
		alwaysMatch,
		auth.DefaultMacHostAuthLimiterConfig()))
	r.POST("/api/v1/host/:id/rotate-key", handler.RotateKey)

	body, err := json.Marshal(map[string]any{"pairing_token": "fresh-token"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/host/"+hostID.String()+"/rotate-key",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", hostID.String())
	req.Header.Set("Authorization", "Bearer test-hash")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, stub.rotateCalls, "service must be invoked exactly once")

	var resp struct {
		Success bool          `json:"success"`
		Error   *api.APIError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.False(t, resp.Success)
	require.NotNil(t, resp.Error)
	require.Equal(t, api.ErrCodeNotFound, resp.Error.Code)
}

// TestRotateKey_ConcurrentRotationLoser_MapsStaleAuthTo401 covers the
// handler's service.ErrAPIKeyStaleAuth → 401 STALE_AUTH mapping: the
// key was rotated out from under the caller between the middleware
// read and the rotate tx's CAS check. The service stub returns the
// sentinel deterministically (the CAS-detection semantics themselves
// are proven at the service layer by
// TestMacHostRotateKey_ConcurrentRotation_DifferentTokens/_SameToken),
// so this pins the wire contract without a real race.
// spec: MAC-010.rotated-out-key-stale-auth
func TestRotateKey_ConcurrentRotationLoser_MapsStaleAuthTo401(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hostID := uuid.New()
	stubHost := &repository.MacHost{
		ID:         hostID,
		Hostname:   "test-host",
		APIKeyHash: "test-hash",
	}
	stub := &stubMacHostService{rotateErr: service.ErrAPIKeyStaleAuth}
	handler := NewMacHostHandler(stub, nil)

	r := gin.New()
	r.Use(auth.MacHostAuthMiddleware(
		&fakeHostRepo{host: stubHost},
		alwaysMatch,
		auth.DefaultMacHostAuthLimiterConfig()))
	r.POST("/api/v1/host/:id/rotate-key", handler.RotateKey)

	body, err := json.Marshal(map[string]any{"pairing_token": "fresh-token"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/host/"+hostID.String()+"/rotate-key",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", hostID.String())
	req.Header.Set("Authorization", "Bearer test-hash")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, stub.rotateCalls, "service must be invoked exactly once")

	var resp struct {
		Success bool          `json:"success"`
		Error   *api.APIError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.False(t, resp.Success)
	require.NotNil(t, resp.Error)
	require.Equal(t, "STALE_AUTH", resp.Error.Code, "loser must surface the literal STALE_AUTH code")
}

// TestHeartbeat_HostRevokedBetweenMiddlewareAndTx covers the
// handler's db.ErrNotFound -> 401 UNKNOWN_HOST branch: the host was
// revoked between the middleware's read and the heartbeat write.
// spec: MAC-011.host-revoked-between-authentication
func TestHeartbeat_HostRevokedBetweenMiddlewareAndTx(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hostID := uuid.New()
	stubHost := &repository.MacHost{
		ID:         hostID,
		Hostname:   "test-host",
		APIKeyHash: "test-hash",
	}
	stub := &stubMacHostService{heartbeatErr: db.ErrNotFound}
	handler := NewMacHostHandler(stub, nil)

	r := gin.New()
	r.Use(auth.MacHostAuthMiddleware(
		&fakeHostRepo{host: stubHost},
		alwaysMatch,
		auth.DefaultMacHostAuthLimiterConfig()))
	r.POST("/api/v1/host/:id/heartbeat", handler.Heartbeat)

	body, err := json.Marshal(map[string]any{
		"daemon_version":   "1.0.0",
		"protocol_version": 1,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/host/"+hostID.String()+"/heartbeat",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", hostID.String())
	req.Header.Set("Authorization", "Bearer test-hash")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, stub.heartbeatCalls, "service must be invoked exactly once")

	var body2 map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body2))
	errObj, ok := body2["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "UNKNOWN_HOST", errObj["code"])
}

// TestRotateKey_MissingPairingToken covers the handler's body
// validation: empty pairing_token returns 400 before the service is
// invoked.
func TestRotateKey_MissingPairingToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hostID := uuid.New()
	stubHost := &repository.MacHost{
		ID:         hostID,
		Hostname:   "test-host",
		APIKeyHash: "test-hash",
	}
	stub := &stubMacHostService{}
	handler := NewMacHostHandler(stub, nil)

	r := gin.New()
	r.Use(auth.MacHostAuthMiddleware(
		&fakeHostRepo{host: stubHost},
		alwaysMatch,
		auth.DefaultMacHostAuthLimiterConfig()))
	r.POST("/api/v1/host/:id/rotate-key", handler.RotateKey)

	body, err := json.Marshal(map[string]any{"pairing_token": ""})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/host/"+hostID.String()+"/rotate-key",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", hostID.String())
	req.Header.Set("Authorization", "Bearer test-hash")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 0, stub.rotateCalls, "service must not be invoked on empty token")
}
