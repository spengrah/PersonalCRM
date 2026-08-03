package handlers

// WhatsAppPairRequest starts a pairing attempt. WhatsApp pairing has no
// user-supplied step to correlate, so there is no session token: at most one
// pairing runs at a time and its live code is read back from the status
// endpoint.
type WhatsAppPairRequest struct {
	// Method is "qr" or "phone".
	Method string `json:"method" binding:"required,oneof=qr phone"`
	// Phone is required when Method is "phone", in E.164 form (+15551234567).
	Phone string `json:"phone" binding:"omitempty"`
}

// WhatsAppPairingResponse is the in-flight pairing attempt.
type WhatsAppPairingResponse struct {
	Method string `json:"method"`
	// QRCode is the raw whatsmeow code string; the client renders the image.
	QRCode *string `json:"qr_code,omitempty"`
	// PairCode is the 8-character code the user types on their phone.
	PairCode  *string `json:"pair_code,omitempty"`
	ExpiresAt string  `json:"expires_at" swaggertype:"string" format:"date-time"`
}

// WhatsAppBackfillResponse reports the one-shot history drain.
type WhatsAppBackfillResponse struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Failed     int `json:"failed"`
	// DroppedInlineChunks counts bootstrap chunks the server delivered with
	// history embedded inline against our explicit non-inline request. Those
	// are dropped un-projected rather than persisted, so a non-zero count is a
	// visible, accepted gap in backfill.
	DroppedInlineChunks int     `json:"dropped_inline_chunks"`
	ObservedFloorAt     *string `json:"observed_floor_at,omitempty" swaggertype:"string" format:"date-time"`
}

// WhatsAppStatusResponse mirrors whatsapp.Status on the wire.
type WhatsAppStatusResponse struct {
	Configured bool `json:"configured"`
	// State is one of not_ready, not_paired, pairing, connecting, connected,
	// reconnecting, disconnected, disconnect_failed, error.
	State string `json:"state"`
	// Reason carries the machine-readable detail for not_ready, disconnected,
	// disconnect_failed and error.
	Reason string `json:"reason,omitempty"`
	// Missing names the dependency the integration is waiting on when State is
	// not_ready, so the settings surface can say what is absent rather than
	// only that something is.
	Missing     string                   `json:"missing,omitempty"`
	JID         *string                  `json:"jid,omitempty"`
	PhoneNumber *string                  `json:"phone_number,omitempty"`
	PushName    *string                  `json:"push_name,omitempty"`
	ConnectedAt *string                  `json:"connected_at,omitempty" swaggertype:"string" format:"date-time"`
	BannedUntil *string                  `json:"banned_until,omitempty" swaggertype:"string" format:"date-time"`
	Pairing     *WhatsAppPairingResponse `json:"pairing,omitempty"`
	// TerminalReasonPersisted is false alongside a disconnected state when the
	// permanent-disconnect reason could not be durably recorded, so the
	// decision will not survive a restart.
	TerminalReasonPersisted bool                     `json:"terminal_reason_persisted,omitempty"`
	Backfill                WhatsAppBackfillResponse `json:"backfill"`
}

// WhatsAppDisconnectResponse reports how an unlink resolved. Disconnect returns
// a body rather than a bare 204 because the forced and already-unlinked paths
// each carry a warning the user needs.
type WhatsAppDisconnectResponse struct {
	// RemoteUnlinked is true when this call logged the device out server-side.
	RemoteUnlinked bool `json:"remote_unlinked"`
	// AlreadyUnlinked is true when the device was confirmed gone server-side
	// before the call, so no remote unlink was attempted.
	AlreadyUnlinked bool `json:"already_unlinked"`
	// Forced is true when local credentials were cleared without server
	// confirmation.
	Forced bool `json:"forced"`
	// Warning is set when the user must finish the unlink from their phone.
	Warning string `json:"warning,omitempty"`
}
