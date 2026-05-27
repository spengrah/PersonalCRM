// AnarlogPathResolver expands the operator-supplied root path into the
// concrete `humans/` and `sessions/` subdirectories the plugins read.
// Pure Foundation; no Files & Folders permission probe (the doctor /
// plugin does that on its own at first touch).
import Foundation

public enum AnarlogPathResolver {
    /// Expand `~` and resolve to an absolute file URL. The CLI
    /// already validates that paths are absolute before persisting,
    /// but this is defense-in-depth: callers that bypass the CLI
    /// (test harnesses, future programmatic config) get the same
    /// normalization.
    public static func expand(_ rootPath: String) -> URL {
        let expanded = (rootPath as NSString).expandingTildeInPath
        return URL(fileURLWithPath: expanded, isDirectory: true)
    }

    /// Returns the `humans/` subdirectory under the configured root.
    public static func humansDir(rootPath: String) -> URL {
        expand(rootPath).appendingPathComponent("humans", isDirectory: true)
    }

    /// Returns the `sessions/` subdirectory under the configured root.
    public static func sessionsDir(rootPath: String) -> URL {
        expand(rootPath).appendingPathComponent("sessions", isDirectory: true)
    }
}

/// Canonical UUID validator. The parent spec requires a
/// case-sensitive lowercase 8-4-4-4-12 hex shape with hyphens for
/// session and human filenames — keeping the input strict matches
/// what Anarlog emits and avoids the ambiguity of cursor keys whose
/// case has been changed by an operator's filesystem case-sensitivity
/// quirks.
public enum AnarlogUUIDValidator {
    /// Returns the (lowercase) UUID string when `s` matches the
    /// case-sensitive lowercase 8-4-4-4-12 hex shape with hyphens;
    /// nil otherwise.
    public static func canonicalize(_ s: String) -> String? {
        // Foundation's UUID(uuidString:) is case-insensitive, so we
        // gate on a pre-check that rejects any uppercase character.
        // Reject early on non-canonical case to keep cursor keys
        // stable across operator filesystem quirks.
        guard s == s.lowercased() else { return nil }
        guard let uuid = UUID(uuidString: s) else { return nil }
        return uuid.uuidString.lowercased()
    }
}
