// Wire DTO for POST /api/v1/host/:id/rotate-key (success).
// Mirrors the Pi-side handlers.rotateKeyResponse struct.
//
// `apiKeyRotatedAt` is decoded as String rather than Date because the
// PiClient's shared JSONDecoder has no date strategy configured (see
// NeedsAttentionEndpoint.swift for the same convention). Changing the
// global decoder strategy would risk breaking every existing decode
// path; the Repairer parses the String to a Date locally via
// ISO8601DateFormatter (with + without fractional seconds, because
// Go's time.Time may emit either depending on monotonic-clock state).
import Foundation

public struct RotateAPIKeyData: Decodable, Equatable, Sendable {
    public let apiKey: String
    public let apiKeyRotatedAt: String

    public init(apiKey: String, apiKeyRotatedAt: String) {
        self.apiKey = apiKey
        self.apiKeyRotatedAt = apiKeyRotatedAt
    }

    enum CodingKeys: String, CodingKey {
        case apiKey = "api_key"
        case apiKeyRotatedAt = "api_key_rotated_at"
    }
}
