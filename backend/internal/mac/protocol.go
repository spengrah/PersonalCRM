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
