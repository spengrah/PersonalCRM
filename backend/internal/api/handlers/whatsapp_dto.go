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
	// Stale reports that these counts could not be refreshed and are the last
	// good values (or zeros, when there has never been a good read). The status
	// endpoint answers during an outage rather than hanging on it, so it says so
	// instead of presenting a fabricated zero as fresh.
	Stale bool `json:"stale,omitempty"`
}

// WhatsAppIngestResponse reports what the live message path observed. The
// counts are a PROCESS-LIFETIME observation, not a persisted total: a restart
// resets them to zero.
type WhatsAppIngestResponse struct {
	// UnresolvedLIDPeers counts distinct peers whose phone number the
	// integration could not recover from their WhatsApp-internal id. Their
	// messages are stored without a contact and can reach one only through the
	// import queue, so this is the visible size of that gap — not a message
	// volume.
	UnresolvedLIDPeers int `json:"unresolved_lid_peers"`
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
	// TerminalReasonPersisted is present only once a terminal disconnect has
	// been taken. False then means the reason could NOT be durably recorded, so
	// the decision will not survive a restart — which is precisely when a client
	// needs to see it. Hence a pointer: a plain bool with omitempty would drop
	// the field in exactly that case, and could not distinguish it from "no
	// terminal decision has been taken".
	TerminalReasonPersisted *bool `json:"terminal_reason_persisted,omitempty"`
	// ReplacedDeviceRetained reports a degraded device store: a re-link could not
	// delete the device it replaced, so a stale session is still stored. The
	// linked account is still resolved deterministically; the flag exists so the
	// condition is visible rather than silent.
	ReplacedDeviceRetained bool `json:"replaced_device_retained,omitempty"`
	// LinkSelectorPersisted is present only once an account has been linked.
	// False then means the record of WHICH device is linked could not be
	// written, so a restart may not resolve it deterministically. The link
	// itself is real and is deliberately not torn down.
	LinkSelectorPersisted *bool                    `json:"link_selector_persisted,omitempty"`
	Backfill              WhatsAppBackfillResponse `json:"backfill"`
	Ingest                WhatsAppIngestResponse   `json:"ingest"`
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
