// Package mac holds shared constants and types for the Pi <-> Mac
// daemon wire protocol. The package is deliberately tiny — it only
// owns version numbers right now — so daemon code and Pi code can
// agree on a contract without dragging in unrelated dependencies.
package mac

// ProtocolVersion is the version the Pi advertises to the daemon in
// heartbeat responses. The daemon compares this against its compiled-in
// value and surfaces a warning to the operator if the Pi is newer than
// the daemon understands.
const ProtocolVersion int32 = 1

// MinProtocolVersion is the minimum daemon protocol_version the Pi
// will accept on heartbeat. A daemon advertising a value below this
// receives 412 Precondition Failed and is expected to upgrade. Bumped
// manually as the contract evolves.
const MinProtocolVersion int32 = 1

// AllowedPushSources is the allowlist of `:source` path-param values
// accepted by the cursor commit + get endpoints. Sources outside this
// set are rejected with 400 before any DB write happens — this
// prevents a daemon from creating a `strategy='push'` row keyed on a
// poll-strategy source name (e.g. "gcontacts"), which would then sit
// in external_sync_state with no provider matching it. New sources
// are added here as their consumers ship.
var AllowedPushSources = map[string]struct{}{
	"messages":        {},
	"icloud_contacts": {},
}

// IsAllowedPushSource returns true when the supplied source is a known
// daemon-push source. Used by handlers to validate the `:source`
// path param before touching the database.
func IsAllowedPushSource(source string) bool {
	_, ok := AllowedPushSources[source]
	return ok
}
