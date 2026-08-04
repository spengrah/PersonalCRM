package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/gin-gonic/gin"
)

// WhatsAppManager is the seam the handler depends on, so the endpoint contract
// can be tested without a whatsmeow client. The production implementation is
// *whatsapp.Manager.
type WhatsAppManager interface {
	Status() wapkg.Status
	// Ready reports whether an account can be linked, and names the missing
	// dependency when it cannot.
	Ready() (bool, string)
	StartPairing(ctx context.Context, req wapkg.PairRequest) error
	CancelPairing()
	Disconnect(ctx context.Context, force bool) (*wapkg.DisconnectResult, error)
}

// WhatsAppChatSettings is the chat-gate slice the handler serves. It is a
// SEPARATE seam from WhatsAppManager because chat-gate rows are ordinary DB
// state the actor does not own — routing them through the actor would put plain
// CRUD on a message queue built for session fencing.
type WhatsAppChatSettings interface {
	ListChats(ctx context.Context) ([]wapkg.ChatWithTracking, error)
	SetChatStatus(ctx context.Context, chatJID, status string) (*wapkg.ChatWithTracking, error)
}

// WhatsAppHandler serves the WhatsApp pairing, status and group-tracking
// surface.
type WhatsAppHandler struct {
	manager WhatsAppManager
	chats   WhatsAppChatSettings
}

// NewWhatsAppHandler creates a new WhatsApp handler. A nil manager or a nil
// chat seam is a supported construction: the affected endpoints report the
// integration unavailable rather than panicking. (In production the routes are
// simply not registered when the feature is off — see registerRoutes — so this
// is defence in depth, and the nil-chats shape is what a boot whose device
// store failed to open actually serves.)
func NewWhatsAppHandler(manager WhatsAppManager, chats WhatsAppChatSettings) *WhatsAppHandler {
	return &WhatsAppHandler{manager: manager, chats: chats}
}

// StartPairing begins linking a WhatsApp account
// @Summary Start WhatsApp pairing
// @Description Begin linking a WhatsApp account by QR code or phone pairing code. The first code is returned in the status body; poll GET /whatsapp/auth/status for refreshed codes.
// @Tags whatsapp
// @Accept json
// @Produce json
// @Param request body WhatsAppPairRequest true "Pairing request"
// @Success 202 {object} api.APIResponse{data=WhatsAppStatusResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 409 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Failure 503 {object} api.APIResponse{error=api.APIError}
// @Failure 504 {object} api.APIResponse{error=api.APIError}
// @Router /whatsapp/auth/start [post]
func (h *WhatsAppHandler) StartPairing(c *gin.Context) {
	if h.manager == nil {
		api.SendError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "WhatsApp integration is not available", "")
		return
	}

	var req WhatsAppPairRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid pairing request", err.Error())
		return
	}
	if req.Method == "phone" && !phoneRegex.MatchString(req.Phone) {
		api.SendValidationError(c, "Invalid phone number", "phone must be E.164, e.g. +15551234567")
		return
	}

	err := h.manager.StartPairing(c.Request.Context(), wapkg.PairRequest{Method: req.Method, Phone: req.Phone})
	switch {
	case err == nil:
		api.SendSuccess(c, http.StatusAccepted, whatsAppStatusResponse(h.manager.Status()), nil)
	case errors.Is(err, wapkg.ErrIngestNotWired):
		// The state every deployment is in until the ingest and history-drain
		// pieces land: pairing is refused rather than linking an account whose
		// messages would be acknowledged and discarded. The details field stays
		// the stable machine code the settings surface branches on; the message
		// names the dependency, which is the actionable half.
		_, missing := h.manager.Ready()
		api.SendError(c, http.StatusConflict, "CONFLICT",
			"WhatsApp is not ready to link an account yet: "+missing, wapkg.ReasonIngestNotWired)
	case errors.Is(err, wapkg.ErrAlreadyConnected):
		api.SendConflict(c, "Already connected — disconnect first")
	case errors.Is(err, wapkg.ErrPairingInProgress):
		api.SendConflict(c, "Pairing already in progress")
	case errors.Is(err, wapkg.ErrUnlinkInProgress):
		// Linking and unlinking are mutually exclusive: "unlink the device while
		// linking a device" has no coherent meaning, and permitting it was what
		// let a purge delete credentials the library had just written.
		api.SendConflict(c, "An unlink is in progress; wait for it to finish before linking an account")
	case errors.Is(err, wapkg.ErrInvalidPhone):
		api.SendValidationError(c, "Invalid phone number", "phone must be E.164, e.g. +15551234567")
	case errors.Is(err, wapkg.ErrUnknownPairMethod):
		api.SendValidationError(c, "Invalid pairing method", "method must be 'qr' or 'phone'")
	case errors.Is(err, wapkg.ErrQRCodeTimeout):
		api.SendError(c, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", "No QR code arrived in time", "qr_code_timeout")
	default:
		api.RespondInternal(c, err)
	}
}

// CancelPairing aborts an in-flight pairing
// @Summary Cancel WhatsApp pairing
// @Description Abort an in-flight WhatsApp pairing attempt. Idempotent.
// @Tags whatsapp
// @Produce json
// @Success 204 "Pairing cancelled"
// @Failure 503 {object} api.APIResponse{error=api.APIError}
// @Router /whatsapp/auth/cancel [post]
func (h *WhatsAppHandler) CancelPairing(c *gin.Context) {
	if h.manager == nil {
		api.SendError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "WhatsApp integration is not available", "")
		return
	}
	h.manager.CancelPairing()
	c.Status(http.StatusNoContent)
}

// StatusClientClosedRequest is nginx's 499: the client closed the connection
// before the server answered. It is not in the RFCs, and it is used here
// deliberately — the alternative, 408, means the client failed to send a request
// in time, which is the opposite of what happened. The request arrived in full
// and the work it started is still running; only the caller left. Keeping it out
// of 5xx is what stops an abandoned request from reading as a server fault.
const StatusClientClosedRequest = 499

// Disconnect unlinks the WhatsApp device
// @Summary Disconnect WhatsApp
// @Description Unlink the WhatsApp device. Local credentials are kept when the remote unlink fails, so the user can retry; ?force=true clears them locally without contacting WhatsApp and returns a warning that the device must be unlinked from the phone.
// @Tags whatsapp
// @Produce json
// @Param force query boolean false "Clear local credentials without contacting WhatsApp"
// @Success 200 {object} api.APIResponse{data=WhatsAppDisconnectResponse}
// @Failure 409 {object} api.APIResponse{error=api.APIError}
// @Failure 499 {object} api.APIResponse{error=api.APIError}
// @Failure 502 {object} api.APIResponse{error=api.APIError}
// @Failure 503 {object} api.APIResponse{error=api.APIError}
// @Router /whatsapp/auth [delete]
func (h *WhatsAppHandler) Disconnect(c *gin.Context) {
	if h.manager == nil {
		api.SendError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "WhatsApp integration is not available", "")
		return
	}

	force := c.Query("force") == "true"
	result, err := h.manager.Disconnect(c.Request.Context(), force)
	switch {
	case err == nil:
		api.SendSuccess(c, http.StatusOK, WhatsAppDisconnectResponse{
			RemoteUnlinked:  result.RemoteUnlinked,
			AlreadyUnlinked: result.AlreadyUnlinked,
			Forced:          result.Forced,
			Warning:         result.Warning,
		}, nil)
	case errors.Is(err, wapkg.ErrNotPaired):
		api.SendConflict(c, "No WhatsApp device is linked")
	case errors.Is(err, wapkg.ErrUnlinkInProgress):
		api.SendConflict(c, "An unlink is already in progress; check the status and retry")
	case errors.Is(err, wapkg.ErrPairingInProgress):
		api.SendConflict(c, "Finish or cancel the pairing before unlinking")
	case errors.Is(err, wapkg.ErrLocalCleanupFailed) && force:
		// A forced clear makes NO remote call, so it produced no evidence about
		// the remote device. Telling the user it was unlinked remotely would be
		// an outright fabrication: it may well still be linked.
		api.SendError(c, http.StatusBadGateway, "BAD_GATEWAY",
			"The stored WhatsApp credentials could not be cleared, and forcing contacts WhatsApp not at all — the device may still be linked. Retry, and remove it from your phone's Linked Devices screen", err.Error())
	case errors.Is(err, wapkg.ErrLocalCleanupFailed):
		// The remote side is settled — the device is unlinked, or no remote call
		// was wanted — and only the LOCAL delete failed. Saying "retry the
		// unlink" would send the user at the half that already worked.
		api.SendError(c, http.StatusBadGateway, "BAD_GATEWAY",
			"The WhatsApp device is unlinked remotely, but the stored credentials could not be cleared; retry to clear them locally", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The CALLER went away; the unlink did not. It is already queued on the
		// actor and runs to completion, so this is not a server fault and must
		// not be counted as one. There is no prior caller-abandoned case in this
		// API, so StatusClientClosedRequest is the convention being set.
		api.SendError(c, StatusClientClosedRequest, "CLIENT_CLOSED_REQUEST",
			"The request was abandoned before the unlink finished; it is still running — check the status", err.Error())
	case errors.Is(err, wapkg.ErrOperationSuperseded):
		// The session this unlink decided about was replaced while it ran, so its
		// outcome was deliberately not published.
		api.SendConflict(c, "The WhatsApp session changed while the unlink was in progress; check the status and retry")
	case errors.Is(err, wapkg.ErrRemoteUnlinkFailed):
		// The device is deliberately KEPT: a failed unlink is not evidence the
		// remote device is gone, and discarding credentials here would orphan
		// a live device on the user's phone.
		api.SendError(c, http.StatusBadGateway, "BAD_GATEWAY",
			"WhatsApp did not confirm the unlink; the device was kept so you can retry", err.Error())
	default:
		api.RespondInternal(c, err)
	}
}

// GetStatus returns the WhatsApp connection status
// @Summary Get WhatsApp status
// @Description Report the WhatsApp connection state, the linked account when paired, any in-flight pairing code, the history-backfill counts, and what the live message path observed. Absent (404) when the feature is disabled.
// @Tags whatsapp
// @Produce json
// @Success 200 {object} api.APIResponse{data=WhatsAppStatusResponse}
// @Failure 503 {object} api.APIResponse{error=api.APIError}
// @Router /whatsapp/auth/status [get]
func (h *WhatsAppHandler) GetStatus(c *gin.Context) {
	if h.manager == nil {
		api.SendError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "WhatsApp integration is not available", "")
		return
	}
	api.SendSuccess(c, http.StatusOK, whatsAppStatusResponse(h.manager.Status()), nil)
}

// ListChats lists the discovered WhatsApp group chats
// @Summary List WhatsApp chats
// @Description List every group chat the ingest gate has observed, with its stored tracking override and the decision the gate would take for it right now. Only group chats are ever recorded; a one-to-one chat never appears.
// @Tags whatsapp
// @Produce json
// @Success 200 {object} api.APIResponse{data=[]WhatsAppChatResponse}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Failure 503 {object} api.APIResponse{error=api.APIError}
// @Router /whatsapp/chats [get]
func (h *WhatsAppHandler) ListChats(c *gin.Context) {
	if h.chats == nil {
		api.SendError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "WhatsApp integration is not available", "")
		return
	}

	chats, err := h.chats.ListChats(c.Request.Context())
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	// make, not var: an empty list must serialise as [] rather than null, or
	// the client's length check reads a missing list as a broken one.
	out := make([]WhatsAppChatResponse, 0, len(chats))
	for _, chat := range chats {
		out = append(out, whatsAppChatToResponse(chat))
	}
	api.SendSuccess(c, http.StatusOK, out, nil)
}

// UpdateChatStatus records the user's per-chat tracking override
// @Summary Update WhatsApp chat tracking
// @Description Set one chat's tracking override to auto, tracked or ignored. The chat must already have been observed — an override is a decision about a discovered chat, never a way to create one — so an unknown chat is a 404. Flipping a chat to tracked does NOT backfill it: WhatsApp history is delivered once, at link time, and messages the gate declined were never stored.
// @Tags whatsapp
// @Accept json
// @Produce json
// @Param chat_jid path string true "Chat JID"
// @Param request body WhatsAppChatStatusRequest true "Tracking override"
// @Success 200 {object} api.APIResponse{data=WhatsAppChatResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Failure 503 {object} api.APIResponse{error=api.APIError}
// @Router /whatsapp/chats/{chat_jid} [patch]
func (h *WhatsAppHandler) UpdateChatStatus(c *gin.Context) {
	if h.chats == nil {
		api.SendError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "WhatsApp integration is not available", "")
		return
	}

	chatJID := c.Param("chat_jid")
	if chatJID == "" {
		api.SendValidationError(c, "Invalid chat", "chat_jid is required")
		return
	}

	var req WhatsAppChatStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid chat status", err.Error())
		return
	}

	updated, err := h.chats.SetChatStatus(c.Request.Context(), chatJID, req.Status)
	if err != nil {
		api.RespondError(c, err, "Chat")
		return
	}
	api.SendSuccess(c, http.StatusOK, whatsAppChatToResponse(*updated), nil)
}

// whatsAppChatToResponse maps one chat-with-decision onto the wire shape.
func whatsAppChatToResponse(chat wapkg.ChatWithTracking) WhatsAppChatResponse {
	return WhatsAppChatResponse{
		ChatJID:          chat.ChatJID,
		ChatTitle:        chat.ChatTitle,
		ChatType:         chat.ChatType,
		MemberCount:      chat.MemberCount,
		Status:           chat.Status,
		EffectiveTracked: chat.EffectiveTracked,
	}
}

// whatsAppStatusResponse maps the manager's status onto the wire shape.
func whatsAppStatusResponse(status wapkg.Status) WhatsAppStatusResponse {
	out := WhatsAppStatusResponse{
		Configured:  status.Configured,
		State:       status.State,
		Reason:      status.Reason,
		Missing:     status.Missing,
		JID:         status.JID,
		PhoneNumber: status.PhoneNumber,
		PushName:    status.PushName,
		ConnectedAt: formatTimePtr(status.ConnectedAt),
		BannedUntil: formatTimePtr(status.BannedUntil),

		TerminalReasonPersisted: status.TerminalReasonPersisted,
		ReplacedDeviceRetained:  status.ReplacedDeviceRetained,
		LinkSelectorPersisted:   status.LinkSelectorPersisted,
		Backfill: WhatsAppBackfillResponse{
			Pending:             status.Backfill.Pending,
			Processing:          status.Backfill.Processing,
			Failed:              status.Backfill.Failed,
			DroppedInlineChunks: status.Backfill.DroppedInlineChunks,
			ObservedFloorAt:     formatTimePtr(status.Backfill.ObservedFloorAt),
			Stale:               status.Backfill.Stale,
		},
		Ingest: WhatsAppIngestResponse{
			UnresolvedLIDPeers: status.Ingest.UnresolvedLIDPeers,
		},
	}
	if status.Pairing != nil {
		out.Pairing = &WhatsAppPairingResponse{
			Method:    status.Pairing.Method,
			QRCode:    status.Pairing.QRCode,
			PairCode:  status.Pairing.PairCode,
			ExpiresAt: status.Pairing.ExpiresAt.UTC().Format(time.RFC3339),
		}
	}
	return out
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
