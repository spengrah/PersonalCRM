// Package mac holds shared constants and types for the Pi <-> Mac
// daemon wire protocol. The package is deliberately tiny — it only
// owns version numbers right now — so daemon code and Pi code can
// agree on a contract without dragging in unrelated dependencies.
package mac

// ProtocolVersion is the version the Pi advertises to the daemon in
// heartbeat responses. The daemon compares this against its compiled-in
// value and surfaces a warning to the operator if the Pi is newer than
// the daemon understands.
//
// v2 (phase 1.5): adds support for call.received / call.sent event
// kinds and the `phone_calls` push source. Older daemons (v1) keep
// working — call.* events are additive, so a v1 daemon that doesn't
// emit them is benignly accepted.
const ProtocolVersion int32 = 2

// MinProtocolVersion is the minimum daemon protocol_version the Pi
// will accept on heartbeat. A daemon advertising a value below this
// receives 412 Precondition Failed and is expected to upgrade. Bumped
// manually as the contract evolves.
//
// Kept at 1 in phase 1.5: existing v1 daemons remain accepted because
// they simply don't emit call.* events. The version bump on the Pi
// side is the signal a v2-aware daemon checks before activating its
// phone_calls plugin.
const MinProtocolVersion int32 = 1

// AllowedPushSources is the allowlist of `:source` path-param values
// accepted by the cursor commit + get endpoints. Sources outside this
// set are rejected with 400 before any DB write happens — this
// prevents a daemon from creating a `strategy='push'` row keyed on a
// poll-strategy source name (e.g. "gcontacts"), which would then sit
// in external_sync_state with no provider matching it. New sources
// are added here as their consumers ship.
var AllowedPushSources = map[string]struct{}{
	"messages":         {},
	"icloud_contacts":  {},
	"anarlog_humans":   {},
	"anarlog_sessions": {},
	"phone_calls":      {},
}

// IsAllowedPushSource returns true when the supplied source is a known
// daemon-push source. Used by handlers to validate the `:source`
// path param before touching the database.
func IsAllowedPushSource(source string) bool {
	_, ok := AllowedPushSources[source]
	return ok
}
