import Foundation

/// Accepts only the lowercase 8-4-4-4-12 form the Anarlog CLI emits and the cursors key on.
public enum AnarlogUUIDValidator {
    /// Returns the lowercase UUID string when `s` is canonical; nil otherwise.
    public static func canonicalize(_ s: String) -> String? {
        guard s == s.lowercased() else { return nil }
        guard let uuid = UUID(uuidString: s) else { return nil }
        return uuid.uuidString.lowercased()
    }
}
