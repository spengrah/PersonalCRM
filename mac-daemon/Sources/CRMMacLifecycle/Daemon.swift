// Daemon-level constants. The label and version are the canonical
// identifiers everything else references — bumped only here.
import Foundation

public enum Daemon {
    /// LaunchAgent label + Keychain service name. Bumped only on a
    /// breaking incompatibility (none planned).
    public static let label = "xyz.spengrah.crm-mac"
    /// Daemon version string. Sent in pair + heartbeat payloads.
    /// Bumped per release; PR6 ships 0.1.0.
    public static let version = "0.1.0"
    /// Protocol version transmitted to the Pi. PR1 floor is 1.
    public static let protocolVersion: Int32 = 1
}
