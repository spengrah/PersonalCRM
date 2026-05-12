package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/mac"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// MacHostService is the narrow interface the handler needs. Tests
// substitute a fake; production wires *service.MacHostService.
type MacHostService interface {
	CreatePairingToken(ctx context.Context) (plaintext string, expiresAt time.Time, err error)
	PairWithToken(ctx context.Context, plaintextToken, hostname, daemonVersion string, protocolVersion int32) (*service.PairResult, error)
	Heartbeat(ctx context.Context, hostID uuid.UUID, payload repository.HeartbeatPayload) (*repository.MacHost, error)
	CommitCursor(ctx context.Context, params repository.CommitMacHostCursorParams) error
	GetCursor(ctx context.Context, source string, hostID uuid.UUID) (string, error)
	ListActiveHosts(ctx context.Context) ([]*repository.MacHost, error)
	GetHost(ctx context.Context, id uuid.UUID) (*repository.MacHost, error)
	RevokeHost(ctx context.Context, id uuid.UUID) error
}

// MacHostHandler handles Mac-daemon HTTP requests + admin UI requests
// against mac_host. Two callsites:
//   - Daemon endpoints (Pair, Heartbeat, GetCursor, CommitCursor, KnownIDs):
//     authenticated via auth.MacHostAuthMiddleware (Pair is unauthenticated
//     but rate-limited per source IP).
//   - Admin endpoints (ListHosts, GetHostAdmin, DeleteHost,
//     CreatePairingToken): authenticated via the existing global API key
//     middleware. Reached from the UI.
type MacHostHandler struct {
	svc            MacHostService
	pairingLimiter *auth.PairingIPRateLimiter
}

// NewMacHostHandler constructs the handler. pairingLimiter is REQUIRED
// for the Pair endpoint; pass auth.NewPairingIPRateLimiter() at wire-up.
func NewMacHostHandler(svc MacHostService, pairingLimiter *auth.PairingIPRateLimiter) *MacHostHandler {
	return &MacHostHandler{svc: svc, pairingLimiter: pairingLimiter}
}

// MacHostView is the JSON shape returned to the admin UI. Pointer fields
// represent nullable DB columns.
type MacHostView struct {
	ID              uuid.UUID       `json:"id"`
	Hostname        string          `json:"hostname"`
	DaemonVersion   string          `json:"daemon_version"`
	ProtocolVersion int32           `json:"protocol_version"`
	LastHeartbeatAt *time.Time      `json:"last_heartbeat_at,omitempty"`
	Permissions     json.RawMessage `json:"permissions"`
	SourceHealth    json.RawMessage `json:"source_health"`
	CursorEpoch     int64           `json:"cursor_epoch"`
	APIKeyRevokedAt *time.Time      `json:"api_key_revoked_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func toMacHostView(h *repository.MacHost) MacHostView {
	permissions := h.Permissions
	if len(permissions) == 0 {
		permissions = json.RawMessage("{}")
	}
	sourceHealth := h.SourceHealth
	if len(sourceHealth) == 0 {
		sourceHealth = json.RawMessage("{}")
	}
	return MacHostView{
		ID:              h.ID,
		Hostname:        h.Hostname,
		DaemonVersion:   h.DaemonVersion,
		ProtocolVersion: h.ProtocolVersion,
		LastHeartbeatAt: h.LastHeartbeatAt,
		Permissions:     permissions,
		SourceHealth:    sourceHealth,
		CursorEpoch:     h.CursorEpoch,
		APIKeyRevokedAt: h.APIKeyRevokedAt,
		CreatedAt:       h.CreatedAt,
		UpdatedAt:       h.UpdatedAt,
	}
}

// pairRequest is the Pair endpoint body.
type pairRequest struct {
	PairingToken    string `json:"pairing_token"`
	Hostname        string `json:"hostname"`
	DaemonVersion   string `json:"daemon_version"`
	ProtocolVersion int32  `json:"protocol_version"`
}

// pairResponse is what the daemon receives on a successful pair.
// api_key is shown ONCE — the daemon must persist it to Keychain.
type pairResponse struct {
	HostID      uuid.UUID `json:"host_id"`
	APIKey      string    `json:"api_key"`
	CursorEpoch int64     `json:"cursor_epoch"`
}

// Pair is the un-authenticated pairing endpoint. Rate-limited per
// source IP to prevent Tailnet-side token-guessing fishing.
func (h *MacHostHandler) Pair(c *gin.Context) {
	if h.pairingLimiter != nil && !h.pairingLimiter.Allow(c.ClientIP()) {
		api.SendError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many pairing attempts", "")
		return
	}

	var req pairRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}
	if req.ProtocolVersion == 0 {
		// Default to 1 when daemon omits — same backward-compat as the heartbeat path.
		req.ProtocolVersion = mac.ProtocolVersion
	}

	res, err := h.svc.PairWithToken(c.Request.Context(), req.PairingToken, req.Hostname, req.DaemonVersion, req.ProtocolVersion)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrPairingTokenInvalid),
			errors.Is(err, service.ErrPairingTokenAlreadyUsed),
			errors.Is(err, service.ErrPairingTokenExpired):
			api.SendError(c, http.StatusGone, "PAIRING_TOKEN_INVALID", err.Error(), "")
			return
		case errors.Is(err, service.ErrHostAlreadyPaired):
			api.SendError(c, http.StatusConflict, "HOST_ALREADY_PAIRED", err.Error(), "")
			return
		}
		logger.Error().Err(err).Msg("pair handler: unexpected error")
		api.SendInternalError(c, "pairing failed")
		return
	}
	api.SendSuccess(c, http.StatusOK, pairResponse{
		HostID:      res.HostID,
		APIKey:      res.APIKey,
		CursorEpoch: res.CursorEpoch,
	}, nil)
}

// heartbeatRequest is the daemon's periodic heartbeat payload.
type heartbeatRequest struct {
	DaemonVersion   string          `json:"daemon_version"`
	ProtocolVersion int32           `json:"protocol_version"`
	Permissions     json.RawMessage `json:"permissions,omitempty"`
	SourceHealth    json.RawMessage `json:"source_health,omitempty"`
}

// heartbeatResponse echoes cursor_epoch so the daemon can detect a Pi
// backup-restore. min_protocol_version is included so a daemon that
// upgrades the Pi out from under itself learns the current floor.
type heartbeatResponse struct {
	OK                 bool  `json:"ok"`
	CursorEpoch        int64 `json:"cursor_epoch"`
	ProtocolVersion    int32 `json:"protocol_version"`
	MinProtocolVersion int32 `json:"min_protocol_version"`
}

// Heartbeat is the authenticated heartbeat endpoint. Returns 412 when
// the daemon's protocol_version is below the floor; daemon-side this
// surfaces as "upgrade required" to the operator.
func (h *MacHostHandler) Heartbeat(c *gin.Context) {
	hostID, ok := auth.MacHostIDFromContext(c)
	if !ok {
		api.SendInternalError(c, "missing mac_host context")
		return
	}
	var req heartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid heartbeat body", err.Error())
		return
	}
	if req.ProtocolVersion < mac.MinProtocolVersion {
		c.AbortWithStatusJSON(http.StatusPreconditionFailed, gin.H{
			"success": false,
			"error": gin.H{
				"code":             "UPGRADE_REQUIRED",
				"message":          "daemon protocol_version below server minimum",
				"min_version":      mac.MinProtocolVersion,
				"upgrade_required": true,
			},
		})
		return
	}

	host, err := h.svc.Heartbeat(c.Request.Context(), hostID, repository.HeartbeatPayload{
		DaemonVersion:   req.DaemonVersion,
		ProtocolVersion: req.ProtocolVersion,
		Permissions:     req.Permissions,
		SourceHealth:    req.SourceHealth,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Host revoked between auth and write — middleware would
			// catch on next request; for this one return 401.
			api.SendError(c, http.StatusUnauthorized, "UNKNOWN_HOST", "host not found or revoked", "")
			return
		}
		logger.Error().Err(err).Msg("heartbeat: update failed")
		api.SendInternalError(c, "heartbeat failed")
		return
	}
	api.SendSuccess(c, http.StatusOK, heartbeatResponse{
		OK:                 true,
		CursorEpoch:        host.CursorEpoch,
		ProtocolVersion:    mac.ProtocolVersion,
		MinProtocolVersion: mac.MinProtocolVersion,
	}, nil)
}

// cursorResponse is the GET cursor body.
type cursorResponse struct {
	Cursor      string `json:"cursor"`
	CursorEpoch int64  `json:"cursor_epoch"`
}

// GetCursor returns the cursor stored for (host, source). Empty string
// + current epoch when nothing has been committed yet.
func (h *MacHostHandler) GetCursor(c *gin.Context) {
	host, ok := auth.MacHostFromContext(c)
	if !ok {
		api.SendInternalError(c, "missing mac_host context")
		return
	}
	source := c.Param("source")
	if source == "" {
		api.SendValidationError(c, "source is required", "")
		return
	}
	cursor, err := h.svc.GetCursor(c.Request.Context(), source, host.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		logger.Error().Err(err).Msg("get cursor: failed")
		api.SendInternalError(c, "get cursor failed")
		return
	}
	api.SendSuccess(c, http.StatusOK, cursorResponse{
		Cursor:      cursor,
		CursorEpoch: host.CursorEpoch,
	}, nil)
}

// commitCursorRequest is the cursor-commit body.
type commitCursorRequest struct {
	Cursor      string `json:"cursor"`
	BaseCursor  string `json:"base_cursor"`
	CursorEpoch int64  `json:"cursor_epoch"`
}

// commitCursorConflict is the 409 response shape. Both
// current_cursor and current_epoch are included so the daemon can
// decide whether to discard local state, fast-forward, or refetch.
type commitCursorConflict struct {
	CurrentCursor *string `json:"current_cursor,omitempty"`
	CurrentEpoch  *int64  `json:"current_epoch,omitempty"`
}

// CommitCursor commits a new cursor under epoch + base_cursor CAS.
// Returns 200 on success, 409 on epoch or base mismatch, 401 on
// revoked-host-mid-tx.
func (h *MacHostHandler) CommitCursor(c *gin.Context) {
	host, ok := auth.MacHostFromContext(c)
	if !ok {
		api.SendInternalError(c, "missing mac_host context")
		return
	}
	source := c.Param("source")
	if source == "" {
		api.SendValidationError(c, "source is required", "")
		return
	}
	var req commitCursorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid commit body", err.Error())
		return
	}
	err := h.svc.CommitCursor(c.Request.Context(), repository.CommitMacHostCursorParams{
		HostID:       host.ID,
		Source:       source,
		BaseCursor:   req.BaseCursor,
		NewCursor:    req.Cursor,
		ClaimedEpoch: req.CursorEpoch,
	})
	if err == nil {
		api.SendSuccess(c, http.StatusOK, gin.H{"ok": true}, nil)
		return
	}

	if errors.Is(err, db.ErrNotFound) {
		api.SendError(c, http.StatusUnauthorized, "UNKNOWN_HOST", "host not found or revoked", "")
		return
	}

	var epochErr *repository.ErrCursorEpochMismatch
	if errors.As(err, &epochErr) {
		epoch := epochErr.ServerEpoch
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "EPOCH_MISMATCH",
				"message": "cursor epoch mismatch",
			},
			"data": commitCursorConflict{CurrentEpoch: &epoch},
		})
		return
	}
	var baseErr *repository.ErrCursorBaseMismatch
	if errors.As(err, &baseErr) {
		cur := baseErr.CurrentCursor
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "BASE_CURSOR_MISMATCH",
				"message": "cursor base mismatch",
			},
			"data": commitCursorConflict{CurrentCursor: &cur},
		})
		return
	}

	logger.Error().Err(err).Msg("commit cursor: unexpected error")
	api.SendInternalError(c, "commit cursor failed")
}

// KnownIDs is the stub endpoint described in §"D4 KnownIDs stub" of the
// plan. PR1 returns an empty list — daemon code is forward-compatible
// because empty IDs is the expected response on a fresh Pi anyway.
// PR5 (external_contact consumer) fills the body in.
func (h *MacHostHandler) KnownIDs(c *gin.Context) {
	api.SendSuccess(c, http.StatusOK, gin.H{"ids": []string{}}, nil)
}

// ListHosts is the admin list view.
func (h *MacHostHandler) ListHosts(c *gin.Context) {
	hosts, err := h.svc.ListActiveHosts(c.Request.Context())
	if err != nil {
		logger.Error().Err(err).Msg("list mac hosts: failed")
		api.SendInternalError(c, "list hosts failed")
		return
	}
	views := make([]MacHostView, 0, len(hosts))
	for _, h := range hosts {
		views = append(views, toMacHostView(h))
	}
	api.SendSuccess(c, http.StatusOK, views, nil)
}

// GetHostAdmin returns a single host (admin view) including revoked.
func (h *MacHostHandler) GetHostAdmin(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "invalid id", err.Error())
		return
	}
	host, err := h.svc.GetHost(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Mac host")
			return
		}
		logger.Error().Err(err).Msg("get mac host: failed")
		api.SendInternalError(c, "get host failed")
		return
	}
	api.SendSuccess(c, http.StatusOK, toMacHostView(host), nil)
}

// DeleteHost is the admin revoke path. Revokes the host's api_key and
// cascades by deleting push-strategy external_sync_state rows. 404 if
// the host is missing or already revoked.
func (h *MacHostHandler) DeleteHost(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "invalid id", err.Error())
		return
	}
	if err := h.svc.RevokeHost(c.Request.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Mac host")
			return
		}
		logger.Error().Err(err).Msg("revoke mac host: failed")
		api.SendInternalError(c, "revoke host failed")
		return
	}
	api.SendSuccess(c, http.StatusOK, gin.H{"ok": true}, nil)
}

// createPairingTokenResponse is the admin "mint token" body. Token
// is shown ONCE.
type createPairingTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreatePairingToken mints a fresh pairing token (admin endpoint).
// The token is returned only once; subsequent admin calls cannot
// recover the plaintext.
func (h *MacHostHandler) CreatePairingToken(c *gin.Context) {
	token, expiresAt, err := h.svc.CreatePairingToken(c.Request.Context())
	if err != nil {
		logger.Error().Err(err).Msg("create pairing token: failed")
		api.SendInternalError(c, "create pairing token failed")
		return
	}
	api.SendSuccess(c, http.StatusOK, createPairingTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
	}, nil)
}
