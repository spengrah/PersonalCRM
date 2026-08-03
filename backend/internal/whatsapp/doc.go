// Package whatsapp holds the in-process WhatsApp integration: the whatsmeow
// client lifecycle, the device store over the application's Postgres pool, the
// pairing state machine, and the durable capture of history-sync
// notifications.
//
// The integration is read-only by design. The only whatsmeow calls it may make
// are NewClient, Connect, Disconnect, Logout, GetQRChannel, PairPhone,
// AddEventHandlerWithSuccessStatus, reads off client.Store, and
// the three history-owned calls DownloadHistorySync, SendProtocolMessageReceipt
// and DeleteMedia. Anything else — sending a message, marking a chat read,
// signalling presence, requesting history, downloading media bytes — is a scope
// violation and is checked mechanically by TestManagerNeverCallsOutboundAPI.
//
// Cleaning up after an INTERRUPTED pairing or unlink is the next boot's job, not
// shutdown's. Stop ends sockets and contexts and nothing else; a device row the
// library wrote for an attempt that a restart cut short is left where it is,
// because the process that is dying cannot see the store as reliably as the one
// that is starting. The resolver reports a store it finds degraded rather than
// guessing which row to resume, the status endpoint surfaces that, and a forced
// disconnect is the documented remedy.
//
// The manager also refuses to connect at all until three wiring facts hold: a
// real message ingestor, a history-notification recorder, and a registered
// history drainer. Until then Status().State is "not_ready" and pairing is
// declined. This is deliberate: WhatsApp delivers a live message once, so a
// client that connects without a durable sink would acknowledge and drop
// messages irrecoverably.
package whatsapp
