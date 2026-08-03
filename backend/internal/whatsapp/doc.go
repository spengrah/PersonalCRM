// Package whatsapp holds the in-process WhatsApp integration: the whatsmeow
// client lifecycle, the device store over the application's Postgres pool, the
// pairing state machine, and the durable capture of history-sync
// notifications.
//
// The integration is read-only by design. The only whatsmeow calls it may make
// are NewClient, Connect, Disconnect, Logout, GetQRChannel, PairPhone,
// AddEventHandlerWithSuccessStatus, GetGroupInfo, reads off client.Store, and
// the three history-owned calls DownloadHistorySync, SendProtocolMessageReceipt
// and DeleteMedia. Anything else — sending a message, marking a chat read,
// signalling presence, requesting history, downloading media bytes — is a scope
// violation and is checked mechanically by TestManagerNeverCallsOutboundAPI.
//
// The manager also refuses to connect at all until three wiring facts hold: a
// real message ingestor, a history-notification recorder, and a registered
// history drainer. Until then Status().State is "not_ready" and pairing is
// declined. This is deliberate: WhatsApp delivers a live message once, so a
// client that connects without a durable sink would acknowledge and drop
// messages irrecoverably.
package whatsapp
