// Package aggregation provides source-neutral burst/session aggregation
// for message streams (Telegram today; iMessage, WhatsApp, etc. in
// upcoming Mac-daemon PRs).
//
// Existing semantics preserved verbatim from the Telegram precedent:
//
//   - Bursts: consecutive same-direction messages within a burst window.
//   - Sessions: bursts merged when adjacent outbound→inbound bridge
//     within the reply-bridge window OR via explicit reply (per-source-
//     defined "reply target" hook).
//   - Two-path output: extend an existing recent interaction OR publish
//     a create-event (message.received / message.sent).
//   - Cross-batch reply bridging via the source's GetMessageByReplyTarget
//     hook (uses the referenced row's InteractionID to find the existing
//     interaction).
//
// Adding a new source: implement SourceAdapter and MessageStore (and
// reuse InteractionFinder, InteractionPromoter, InteractionExtender as
// satisfied by the existing repository / service types). The Engine is
// instantiated per source — concurrent engines for telegram + messages
// + whatsapp share no mutable state.
//
// The engine does NOT read "now"; window math uses message timestamps
// only. Do NOT introduce a time.Now() / accelerated.GetCurrentTime()
// call inside this package; preserving timestamp-driven math is part of
// the semantics contract.
package aggregation
