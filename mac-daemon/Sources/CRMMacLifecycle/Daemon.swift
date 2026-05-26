// Daemon-level constants. The label and version are the canonical
// identifiers everything else references — bumped only here.
import Foundation

public enum Daemon {
    /// LaunchAgent label + Keychain service name. Bumped only on a
    /// breaking incompatibility (none planned).
    public static let label = "xyz.spengrah.crm-mac"
    /// Daemon version string. Sent in pair + heartbeat payloads.
    /// Bumped per release.
    public static let version = "0.1.0"
    /// Protocol version transmitted to the Pi. Bumped to 2 when the
    /// daemon started emitting `call.received` / `call.sent` events
    /// (Phase 1.5 phone_calls source). The Pi accepts both v1 and v2
    /// daemons — this is the daemon's self-declaration only.
    public static let protocolVersion: Int32 = 2
}
