package whatsapp

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
)

// Connection states reported by Status().State.
const (
	// StateNotReady is the readiness gate: one of the three wiring facts is
	// missing, so the client will not connect and pairing is declined.
	StateNotReady = "not_ready"
	// StateNotPaired means no linked device is stored.
	StateNotPaired = "not_paired"
	// StatePairing means a pairing attempt is in flight.
	StatePairing = "pairing"
	// StateConnecting means a connection has been requested but not confirmed.
	StateConnecting = "connecting"
	// StateConnected means the linked session is live.
	StateConnected = "connected"
	// StateReconnecting means the socket dropped and whatsmeow is retrying on
	// its own. No user action is required.
	StateReconnecting = "reconnecting"
	// StateDisconnected is the terminal class: the reason is durable and no
	// reconnect is attempted, across restarts.
	StateDisconnected = "disconnected"
	// StateDisconnectFailed means an unlink was requested, the remote logout
	// failed, and local credentials were deliberately KEPT so the user can
	// retry (or force a local-only clear).
	StateDisconnectFailed = "disconnect_failed"
	// StateError means the stack could not be brought up.
	StateError = "error"
)

// Terminal disconnect reasons. These are persisted into
// external_sync_state.metadata so the "do not reconnect" decision survives a
// restart, and they are the machine-readable values the settings UI branches
// on.
const (
	ReasonLoggedOut      = "logged_out"
	ReasonStreamReplaced = "stream_replaced"
	ReasonClientOutdated = "client_outdated"
	ReasonTemporaryBan   = "temporary_ban"
	// ReasonIngestNotWired is the readiness-gate reason; it is not terminal.
	ReasonIngestNotWired = "ingest_not_wired"
	// ReasonLocalCleanupFailed reports that the remote device is gone — either
	// WhatsApp unlinked it or our unlink succeeded — but the LOCAL credentials
	// could not be deleted. It is not terminal and it is not a failed unlink:
	// the remedy is retrying the local clear, never retrying the unlink, which
	// against an already-unlinked device cannot succeed.
	ReasonLocalCleanupFailed = "local_cleanup_failed"
	// ReasonDeviceStoreAmbiguous reports that the device store holds two or more
	// sets of credentials and none of them is the account the integration
	// recorded as linked. Nothing is resumed; the remedy is a forced disconnect,
	// which purges the enumerated set and returns the store to a resolvable
	// state.
	ReasonDeviceStoreAmbiguous = "device_store_ambiguous"
	// ReasonForcedCleanupFailed reports that a FORCED local-only clear failed.
	// It is deliberately distinct from ReasonLocalCleanupFailed: force makes no
	// remote call, so it produces no evidence about the remote device, and a
	// later unlink must still try the remote half rather than assuming it is
	// already done.
	ReasonForcedCleanupFailed = "forced_cleanup_failed"
	// ReasonScannedWithoutMultidevice reports that the code WAS scanned but the
	// phone has multi-device mode off. It is not terminal and not a failure: the
	// library keeps emitting codes, so the remedy is to turn multi-device on and
	// scan the next one. Surfaced because a silent retry looks identical to a
	// scan that did nothing.
	ReasonScannedWithoutMultidevice = "scanned_without_multidevice"
	// ReasonPasskeyPairingUnsupported reports that the account asked to complete
	// pairing with a passkey handoff. The integration has no surface for that
	// exchange, so the attempt is ended with a reason the user can act on rather
	// than left to run out of codes.
	ReasonPasskeyPairingUnsupported = "passkey_pairing_unsupported"
)

// Pairing methods accepted by StartPairing.
const (
	PairMethodQR    = "qr"
	PairMethodPhone = "phone"
)

var (
	// ErrIngestNotWired is returned by Start and StartPairing while the
	// readiness gate is unsatisfied. It is deliberately also what the default
	// message ingestor returns: a no-op that SUCCEEDS would acknowledge and
	// drop a live message irrecoverably, while a no-op that FAILS withholds
	// the ack and lets WhatsApp redeliver.
	ErrIngestNotWired = errors.New("whatsapp: message ingest is not wired")

	// ErrAlreadyConnected is returned when pairing is requested on a live
	// session.
	ErrAlreadyConnected = errors.New("whatsapp: already connected")

	// ErrPairingInProgress is returned when a second pairing is requested
	// while one is in flight. At most one pairing runs at a time.
	ErrPairingInProgress = errors.New("whatsapp: pairing already in progress")

	// ErrInvalidPhone is returned when a phone-code pairing is requested with
	// a number that is not E.164.
	ErrInvalidPhone = errors.New("whatsapp: phone number must be E.164")

	// ErrUnknownPairMethod is returned for a method other than qr or phone.
	ErrUnknownPairMethod = errors.New("whatsapp: unknown pairing method")

	// ErrQRCodeTimeout is returned when the pairing websocket produces no QR
	// item inside qrFirstCodeTimeout. It applies to BOTH methods: the library
	// generates QR codes for phone-code pairing too, and the first one is its
	// documented signal that the connection is fully established. The pairing is
	// torn down before this is returned.
	ErrQRCodeTimeout = errors.New("whatsapp: no QR code arrived in time")

	// ErrPairingCancelled is returned when an attempt was cancelled while its
	// client was still being built. The half-built client is discarded rather
	// than connected — an orphaned connected client would be unreachable by
	// Stop() and could still complete a pairing nothing recorded.
	ErrPairingCancelled = errors.New("whatsapp: pairing was cancelled")

	// ErrPasskeyPairingUnsupported ends an attempt the account wants to finish
	// with a passkey handoff, which this integration has no surface for.
	ErrPasskeyPairingUnsupported = errors.New("whatsapp: this account requires passkey pairing, which is not supported")

	// ErrNotPaired is returned by Disconnect when there is nothing to unlink.
	ErrNotPaired = errors.New("whatsapp: no linked device")

	// ErrRemoteUnlinkFailed is returned by Disconnect when the remote logout
	// (or the connect made solely to log out) failed. Local credentials are
	// KEPT — a failed connect is not evidence that the device is unlinked.
	ErrRemoteUnlinkFailed = errors.New("whatsapp: remote unlink failed")

	// ErrLocalCleanupFailed is returned when the remote side needed no further
	// action — already unlinked, just unlinked, or a forced local-only clear —
	// but the stored device could not be deleted. Distinct from
	// ErrRemoteUnlinkFailed because the user action differs: retry the clear,
	// not the unlink.
	ErrLocalCleanupFailed = errors.New("whatsapp: local credentials could not be cleared")

	// ErrOperationSuperseded is returned when a multi-step operation (an unlink,
	// a pairing) found that the session or pairing it decided about was replaced
	// while it was in flight. The operation aborts rather than applying its
	// decision to state that is no longer the state it decided about.
	ErrOperationSuperseded = errors.New("whatsapp: the session changed while the operation was in flight")

	// ErrUnlinkInProgress is returned when a second unlink is requested while one
	// is in flight, and when a pairing is requested during one. Two overlapping
	// unlinks were always incoherent — two remote logouts, two publications, one
	// device — and link/unlink interleaving is what let a purge delete a device
	// the library had just saved.
	ErrUnlinkInProgress = errors.New("whatsapp: an unlink is already in progress")

	// ErrDeviceStoreAmbiguous is returned when the device store holds two or
	// more devices and none of them is the one the linked-account selector
	// names. Nothing is resumed: there is no code path that returns an arbitrary
	// row from a multi-row store.
	ErrDeviceStoreAmbiguous = errors.New("whatsapp: the device store is ambiguous")

	// ErrManagerStopped is returned to a caller whose operation could not be
	// completed because the manager is shutting down.
	ErrManagerStopped = errors.New("whatsapp: the manager is stopping")

	// ErrHistoryNotificationMalformed is returned when a stored history media
	// pointer cannot be unmarshalled. The drainer treats it as PERMANENT: the
	// stored bytes are what they are, and no retry re-decodes them differently.
	ErrHistoryNotificationMalformed = errors.New("whatsapp: history notification is malformed")

	// ErrLIDMappingsIncomplete is returned by the history fetcher when a
	// downloaded chunk's PN-LID mappings did not read back out of the client
	// store. The drainer treats it as TRANSIENT: whatsmeow logs and swallows
	// its own mapping-store failures while still reporting the download as a
	// success, so the read-back is the only real guarantee, and projecting
	// without it would mis-attribute every message from a LID-only peer.
	ErrLIDMappingsIncomplete = errors.New("whatsapp: history LID mappings incomplete")
)

// Status is the connection + pairing + backfill snapshot the settings surface
// renders.
type Status struct {
	// Configured reports whether ENABLE_WHATSAPP_SYNC is on. It is false only
	// on a manager that was never built, which the handler never sees.
	Configured bool `json:"configured"`
	// State is one of the State* constants.
	State string `json:"state"`
	// Reason carries the machine-readable detail for not_ready, disconnected,
	// disconnect_failed and error.
	Reason string `json:"reason,omitempty"`
	// Missing names the human-readable dependency the readiness gate is waiting
	// on, when State is not_ready. Reason stays the stable machine-readable
	// code; this is what tells the operator WHICH piece is absent, which is the
	// whole point of reporting not_ready rather than a bare error.
	Missing     string     `json:"missing,omitempty"`
	JID         *string    `json:"jid,omitempty"`
	PhoneNumber *string    `json:"phone_number,omitempty"`
	PushName    *string    `json:"push_name,omitempty"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	BannedUntil *time.Time `json:"banned_until,omitempty"`
	Pairing     *Pairing   `json:"pairing,omitempty"`
	// TerminalReasonPersisted reports whether a terminal disconnect was durably
	// recorded. It is a POINTER because the field is only meaningful alongside a
	// terminal state: nil means "no terminal decision has been taken", while a
	// non-nil false means the decision was taken and could NOT be recorded, so
	// it will not survive a restart. A plain bool could not tell those apart —
	// and with omitempty it would vanish in exactly the case a client has to see.
	TerminalReasonPersisted *bool `json:"terminal_reason_persisted,omitempty"`
	// ReplacedDeviceRetained reports a degraded device store: a re-pair adopted
	// a new device but could not delete the one it replaced, so the store holds
	// more than the single session the library's first-device lookup assumes.
	// The linked device is still resolved deterministically (by JID), but the
	// stale row is real and is surfaced rather than logged and forgotten.
	ReplacedDeviceRetained bool `json:"replaced_device_retained,omitempty"`
	// LinkSelectorPersisted reports whether the record of WHICH device is linked
	// survived the adoption that created it. It is a pointer for the same reason
	// TerminalReasonPersisted is: nil means no adoption has happened, while a
	// non-nil false means one did and the record of which device did not survive.
	// The pairing is deliberately NOT torn down in that case — the device is
	// genuinely linked remotely.
	LinkSelectorPersisted *bool `json:"link_selector_persisted,omitempty"`
	// Backfill reports the history drain. PR3 records notifications; the
	// counts stay zero until a chunk arrives.
	Backfill BackfillStatus `json:"backfill"`
	// Ingest reports what the live message path observed.
	Ingest IngestStatus `json:"ingest"`
}

// clone deep-copies every pointer target.
//
// A shallow copy would publish pointers into loop-owned memory, so a caller
// mutating *status.JID would corrupt the published snapshot and race the loop.
// It is applied in BOTH directions — publish clones before storing and Status
// clones what it read — because either alone leaves a mutable path: publish-only
// lets two concurrent readers share targets, read-only lets the loop keep
// aliases into what it published.
//
// IngestStatus holds no pointers, so the exhaustiveness claim above stays true
// without a line for it.
func (s Status) clone() Status {
	out := s
	out.JID = cloneStringPtr(s.JID)
	out.PhoneNumber = cloneStringPtr(s.PhoneNumber)
	out.PushName = cloneStringPtr(s.PushName)
	out.ConnectedAt = cloneTimePtr(s.ConnectedAt)
	out.BannedUntil = cloneTimePtr(s.BannedUntil)
	out.TerminalReasonPersisted = cloneBoolPtr(s.TerminalReasonPersisted)
	out.LinkSelectorPersisted = cloneBoolPtr(s.LinkSelectorPersisted)
	out.Backfill.ObservedFloorAt = cloneTimePtr(s.Backfill.ObservedFloorAt)
	if s.Pairing != nil {
		p := *s.Pairing
		p.QRCode = cloneStringPtr(s.Pairing.QRCode)
		p.PairCode = cloneStringPtr(s.Pairing.PairCode)
		out.Pairing = &p
	}
	return out
}

func cloneStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneTimePtr(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// BackfillStatus is read from WhatsAppRepository.
type BackfillStatus struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Failed     int `json:"failed"`
	// DroppedInlineChunks counts chunks the server delivered with history
	// embedded inline, against our explicit non-inline request. Those are
	// dropped un-projected rather than persisted, because persisting them
	// would store pre-clamp message content. A non-zero count is a visible,
	// accepted gap in backfill.
	DroppedInlineChunks int        `json:"dropped_inline_chunks"`
	ObservedFloorAt     *time.Time `json:"observed_floor_at,omitempty"`
	// Stale reports that these counts could not be refreshed — the read timed
	// out or failed — so they are the last good values (or zeros, when there has
	// never been a good read). The status endpoint is what an operator hits
	// DURING an outage, so it degrades the answer rather than hanging, and says
	// so rather than presenting a fabricated zero as fresh.
	Stale bool `json:"stale,omitempty"`
}

// IngestStatus is what the live message path observed since this process
// started. It is deliberately NOT persisted: it reports the current process's
// view, so a restart resets it to zero.
type IngestStatus struct {
	// UnresolvedLIDPeers counts DISTINCT peers OBSERVED whose phone number
	// could not be recovered from their LID. Such a peer cannot be matched to a
	// contact automatically, so any message of theirs that IS stored is stored
	// without one and can reach a contact only through the import queue.
	//
	// It counts peers observed, NOT peers whose messages were stored: a sender
	// in a group the size gate declines to track is counted too. The number is
	// therefore how much of the conversation graph the integration cannot
	// attribute on its own — not a count of unattributed rows, and not a
	// message volume. It saturates at maxUnresolvedLIDPeers.
	UnresolvedLIDPeers int `json:"unresolved_lid_peers"`
}

// Pairing is the in-flight pairing attempt. There is no session token: only one
// pairing runs at a time and its state is read back through the status
// endpoint.
type Pairing struct {
	Method string `json:"method"`
	// QRCode is the raw whatsmeow code string. The client renders the image;
	// keeping it a string keeps an image dependency out of the backend.
	QRCode *string `json:"qr_code,omitempty"`
	// PairCode is the 8-character code the user types on their phone.
	PairCode  *string   `json:"pair_code,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PairRequest is the input to StartPairing.
type PairRequest struct {
	Method string
	Phone  string
}

// DisconnectResult reports how an unlink resolved. Disconnect returns a body
// rather than a bare 204 because the force and already-unlinked paths both
// carry a warning the user needs.
type DisconnectResult struct {
	// RemoteUnlinked is true when this call logged the device out server-side.
	RemoteUnlinked bool `json:"remote_unlinked"`
	// AlreadyUnlinked is true when a durable logged_out reason proved the
	// device was already gone server-side, so no remote call was made.
	AlreadyUnlinked bool `json:"already_unlinked"`
	// Forced is true when local state was cleared without server confirmation.
	Forced bool `json:"forced"`
	// Warning is set when the user must finish the job from their phone.
	Warning string `json:"warning,omitempty"`
}

// IngestedMessage is the source-agnostic projection of a WhatsApp message.
// whatsmeow types never leak past the parser.
type IngestedMessage struct {
	MessageID     string
	ChatJID       string
	ChatType      string // "private" | "group"
	ChatTitle     *string
	IsOutgoing    bool
	SentAt        time.Time
	Body          *string
	MessageType   string // text|photo|audio|video|document|sticker|other
	ReplyTargetID *string
	PeerJID       *string // nil for outbound group messages
	PeerPhoneE164 *string // nil when unresolvable
	PushName      *string
	MemberCount   *int // groups only
	// AccountJID is the EMITTING session's own JID, in non-AD form. It is
	// carried per event rather than read from the published snapshot: during a
	// re-pair a snapshot read would stamp a retired session's in-flight message
	// with the new account's JID, and the non-AD form is what stops the device
	// number a re-link reassigns from fragmenting account_id.
	AccountJID *string
}

// ChatGroupInfo is a group's metadata, projected off whatsmeow's types.
type ChatGroupInfo struct {
	Title       string
	MemberCount int
}

// GroupInfoFetcher is the seam the group gate uses to reach the live client for
// the one metadata call this integration makes. It is defined on the manager so
// the gate never holds a *whatsmeow.Client.
type GroupInfoFetcher interface {
	GroupInfo(ctx context.Context, chatJID string) (*ChatGroupInfo, error)
	// AccountJID reports which linked account this fetcher's client belongs to,
	// in the same non-AD form IngestedMessage.AccountJID carries, or "" when it
	// cannot be determined.
	//
	// It exists because the fetcher is resolved from the PUBLISHED session while
	// a message is parsed against its EMITTING one. During an overlapping
	// re-pair those differ, and asking the new account about a group only the
	// old account was in answers "not in that group" — which the gate treats as
	// a permanent decision and would therefore acknowledge and drop the message
	// for good. Comparing the two identities turns that into a withheld ack.
	AccountJID() string
}

// GroupInfoBinder is implemented by an ingestor that needs to reach the live
// client for group metadata. SetIngestor binds it, so an ingestor cannot be
// installed without its source — there is no window in which one is installed
// and the other is not.
type GroupInfoBinder interface {
	BindGroupInfoSource(src func() GroupInfoFetcher)
}

// MessageIngestor is the projected-message path: live messages now, and the
// history drainer replays through the SAME method so backfill and live ingest
// cannot diverge. A non-nil error withholds the ack, so WhatsApp redelivers.
type MessageIngestor interface {
	IngestMessage(ctx context.Context, msg IngestedMessage) error
}

// HistoryNotificationRecorder is the raw history path. It MUST be synchronous,
// MUST persist before returning, and MUST NOT download or project. It stores a
// media POINTER, never message content. There is no no-op variant, because a
// no-op here is silent, unrecoverable history loss.
type HistoryNotificationRecorder interface {
	RecordHistoryNotification(ctx context.Context, protocolMsgID string, notification []byte, syncType string, chunkOrder int32, oldestMsgTS *time.Time, disposition string) error
}

// HistoryFetcher is the seam the history drainer uses to reach the live client
// for the three calls it cannot make without one. It is defined on the manager
// so the drainer never holds a *whatsmeow.Client.
type HistoryFetcher interface {
	// DownloadHistorySync downloads with synchronousStorage=true and then
	// verifies, inside this implementation, that every PN-LID mapping in the
	// chunk reads back out of the client's own LID store. Verification lives
	// here rather than in the caller for two reasons: the drainer has no
	// client, and putting it behind the same call makes it impossible to
	// forget. Returns ErrLIDMappingsIncomplete when any expected mapping is
	// absent or maps to a different phone number.
	DownloadHistorySync(ctx context.Context, notification []byte) (*waHistorySync.HistorySync, error)
	// AckHistorySync sends the protocol receipt whatsmeow would otherwise
	// have sent unconditionally on delivery. We move WHEN it fires, not
	// whether.
	AckHistorySync(ctx context.Context, protocolMsgID string) error
	// DeleteHistoryMedia removes our own history payload from WhatsApp's
	// media server. It acts on our blob, never on a conversation partner's
	// state.
	DeleteHistoryMedia(ctx context.Context, notification []byte) error

	// AccountJID reports the linked account this fetcher's client belongs to,
	// in the same non-AD form IngestedMessage.AccountJID carries, or "" when it
	// cannot be determined.
	//
	// Same contract, same derivation and same reason as
	// GroupInfoFetcher.AccountJID: the group gate must be able to refuse to ask
	// the wrong account about a group, and the drainer is the caller that hands
	// it the account to compare against.
	AccountJID() string

	// ProjectHistoryMessage projects ONE history message onto IngestedMessage
	// through the SAME parser the live path uses, so backfill and live ingest
	// cannot diverge in attribution, eligibility, direction or account stamp.
	//
	// It lives behind this seam rather than in the drainer because projection
	// needs three things only the client has: the account's own JIDs, the
	// device store's LID-to-phone map, and the library's own WebMessageInfo
	// decode. The drainer holds no *whatsmeow.Client, so it cannot have them.
	//
	// eligible=false means DROP with no error — an ineligible chat or a
	// non-conversational turn. A non-nil error is a decode failure for that ONE
	// message and is never a chunk failure.
	ProjectHistoryMessage(ctx context.Context, chatJID string, webMsg *waWeb.WebMessageInfo) (msg IngestedMessage, eligible bool, err error)
}

// refusingIngestor is the default MessageIngestor. It refuses rather than
// succeeding, so that even if the readiness gate were bypassed the handler
// returns false, the ack is withheld, and WhatsApp redelivers. A no-op that
// SUCCEEDS is the shape that loses data.
type refusingIngestor struct{}

func (refusingIngestor) IngestMessage(context.Context, IngestedMessage) error {
	return ErrIngestNotWired
}
