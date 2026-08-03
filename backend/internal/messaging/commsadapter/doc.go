// Package commsadapter holds the source-parameterized glue that binds a
// comms_message-backed chat source to the shared burst/session aggregator in
// personal-crm/backend/internal/messaging/aggregation.
//
// What belongs here: anything a comms_message-backed source needs that differs
// from another such source ONLY by its source string and its human label. That
// is the Adapter (wire formats), the StoreAdapter (comms_message →
// aggregation.Message projection), the InteractionFinder/EventLookup/Publisher
// wrappers, and the engine constructor. What does NOT belong here: per-source
// staging tables, per-source parsing, or anything that reads a column no other
// comms source writes.
//
// Two rules this package exists to enforce structurally rather than by
// convention:
//
//  1. SourceName() == the SourceRef/SourceRefPrefix/PeerRef prefix. The
//     consumer's CommsAggregatorReenqueuer recovers a chat id by stripping
//     source + ":" from the event PeerRef, so a source whose ref prefix differs
//     from its source string silently loses post-record re-aggregation. Adapter
//     derives all three from one field, so they cannot drift. (Telegram's ref
//     prefix is "tg:" while its source is "telegram", which is exactly why
//     Telegram cannot use Adapter and keeps its own.)
//
//  2. An unavailable *events.Bus must reach the engine as the UNTYPED-nil
//     interface value. A typed-nil concrete pointer satisfies the engine's
//     `publisher == nil` guard as non-nil and silently bypasses it — see the
//     EventPublisher note in messaging/aggregation/interfaces.go. EventLookup
//     and Publisher return a literal nil interface for a nil bus, so no caller
//     has to re-implement the conversion.
//
// The repository package stays free of aggregation-package imports; that is why
// the row projection lives here rather than beside repository.CommsMessage.
package commsadapter
