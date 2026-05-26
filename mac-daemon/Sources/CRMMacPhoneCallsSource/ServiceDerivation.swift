// ServiceDerivation — pure function mapping
// (ZSERVICE_PROVIDER, ZCALLTYPE) -> CallHistoryDB service enum.
//
// The matrix is FROZEN per spec §`phone_calls` source:
//
//   | ZSERVICE_PROVIDER       | ZCALLTYPE | Service          |
//   |-------------------------|-----------|------------------|
//   | com.apple.Telephony     | (any)     | voice            |
//   | com.apple.FaceTime      | 8         | facetime_audio   |
//   | com.apple.FaceTime      | 16        | facetime_video   |
//   | (anything else)         | (any)     | nil (rejected)   |
//
// nil means the row is rejected by the reader (logged + counted as
// service_unknown; not forwarded to the Pi). Adding a new service is a
// coordinated change: daemon mapping + Pi-side migration adding a
// CHECK constraint value.
import Foundation

/// Canonical service enum that mirrors the Pi-side
/// phone_call.service CHECK constraint.
public enum PhoneCallService: String, Sendable, Equatable {
    case voice
    case facetimeAudio = "facetime_audio"
    case facetimeVideo = "facetime_video"
}

public enum ServiceDerivation {
    /// Returns the canonical service for a (ZSERVICE_PROVIDER, ZCALLTYPE)
    /// pair, or nil if the combination is not in the frozen matrix.
    ///
    /// `provider` is matched case-insensitively because Apple's
    /// CallHistoryDB has been observed to vary capitalization across
    /// macOS releases on the `com.apple.*` reverse-DNS strings.
    public static func resolve(provider: String?, callType: Int64?) -> PhoneCallService? {
        guard let provider, !provider.isEmpty else {
            return nil
        }
        let normalizedProvider = provider.lowercased()
        switch normalizedProvider {
        case "com.apple.telephony":
            // Telephony rows are always voice — ZCALLTYPE is not
            // consulted (it's used by Apple for internal subtypes that
            // don't affect the service-tier mapping).
            return .voice
        case "com.apple.facetime":
            switch callType {
            case 8:  return .facetimeAudio
            case 16: return .facetimeVideo
            default: return nil
            }
        default:
            return nil
        }
    }
}
