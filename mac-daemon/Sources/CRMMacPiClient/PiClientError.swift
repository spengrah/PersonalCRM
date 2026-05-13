// Typed error surface for the Pi client. Workflows in CRMMacLifecycle
// switch on these to map to exit codes (per plan D14) or operator
// messages.
import Foundation

public enum PiClientError: Error, Equatable, CustomStringConvertible {
    /// 410 PAIRING_TOKEN_INVALID. Token was invalid, expired, or
    /// already used — the Pi deliberately returns one opaque code.
    case pairingTokenRejected(message: String)
    /// 409 HOST_ALREADY_PAIRED. Operator must revoke the existing
    /// host via `crm-admin --list-hosts` + `--revoke-host` before
    /// re-pair.
    case hostAlreadyPaired(message: String)
    /// 401 (UNKNOWN_HOST or AUTH_REVOKED). Daemon must exit 1 per D14.
    case authenticationRevoked(message: String)
    /// 412 UPGRADE_REQUIRED. Pi has bumped the protocol-version floor
    /// above what this daemon supports. Exit 2 per D14.
    case upgradeRequired(minVersion: Int32?, message: String)
    /// 4xx other than 401/409/410/412. Surfaced verbatim — no retry.
    case clientError(status: Int, code: String, message: String)
    /// 5xx after RetryingTransport has exhausted retries.
    case serverError(status: Int, message: String)
    /// Network/transport failure (timeout, DNS, TLS) after retries.
    case transport(underlying: String)
    /// Response did not parse as a Pi envelope.
    case decode(reason: String)
    /// Endpoint returned `success=false` but no recognizable error
    /// code. Should not happen against the current Pi but we surface
    /// it explicitly rather than silently treating as success.
    case envelopeError(code: String, message: String)

    public var description: String {
        switch self {
        case .pairingTokenRejected(let m): return "pairing token rejected: \(m)"
        case .hostAlreadyPaired(let m): return "host already paired: \(m)"
        case .authenticationRevoked(let m): return "authentication revoked: \(m)"
        case .upgradeRequired(let v, let m):
            return "upgrade required (min protocol version=\(v.map(String.init) ?? "unknown")): \(m)"
        case .clientError(let s, let c, let m): return "client error \(s) \(c): \(m)"
        case .serverError(let s, let m): return "server error \(s): \(m)"
        case .transport(let u): return "transport error: \(u)"
        case .decode(let r): return "decode error: \(r)"
        case .envelopeError(let c, let m): return "envelope error \(c): \(m)"
        }
    }
}
