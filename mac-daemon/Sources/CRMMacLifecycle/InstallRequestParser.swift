// InstallRequestParser turns the CLI-parsed string options into an
// InstallRequest, applying the front-loaded validation that would
// otherwise surface mid-install as a confusing transport error.
//
// Lives in CRMMacLifecycle (not the executable target) so it can be
// unit-tested — the executable target has no test target by design.
import Foundation
import CRMMacCore

public struct InstallRequestParserInput: Equatable {
    public let piURL: String
    public let pair: String
    public let hostname: String
    public let upgrade: Bool
    public let registerOnly: Bool

    public init(piURL: String, pair: String, hostname: String, upgrade: Bool, registerOnly: Bool) {
        self.piURL = piURL
        self.pair = pair
        self.hostname = hostname
        self.upgrade = upgrade
        self.registerOnly = registerOnly
    }
}

public enum InstallRequestParseError: Error, Equatable, CustomStringConvertible {
    case mutuallyExclusiveModes
    case piURLRequired
    case pairTokenRequired
    case hostnameRequired
    case malformedPiURL(raw: String)
    case invalidPiURL(reason: String)

    public var description: String {
        switch self {
        case .mutuallyExclusiveModes:
            return "--upgrade and --register-only are mutually exclusive"
        case .piURLRequired:
            return "--pi-url <url> is required for fresh install"
        case .pairTokenRequired:
            return "--pair <token> is required for fresh install"
        case .hostnameRequired:
            return "--hostname <label> is required. Pick a non-PII label like 'mac-1', 'work-mac', 'home-laptop'."
        case .malformedPiURL(let raw):
            return "--pi-url is not a valid URL: \(raw)"
        case .invalidPiURL(let reason):
            return "--pi-url is invalid: \(reason)"
        }
    }
}

/// Captures non-fatal warnings the parser wants to surface to the
/// operator. The InstallCommand prints these to stderr.
public struct InstallRequestParseWarnings: Equatable {
    /// True when a fresh install passes `http://` to a non-loopback
    /// host. The API key is sent in a Bearer header on every
    /// authenticated request — plaintext over the wire to a routable
    /// host leaks the credential to anyone in the network path.
    public var plaintextHTTPNonLoopback: Bool

    public init(plaintextHTTPNonLoopback: Bool = false) {
        self.plaintextHTTPNonLoopback = plaintextHTTPNonLoopback
    }

    public var isEmpty: Bool {
        !plaintextHTTPNonLoopback
    }
}

public enum InstallRequestParser {
    public static func parse(_ input: InstallRequestParserInput) throws -> InstallRequest {
        return try parseWithWarnings(input).request
    }

    public static func parseWithWarnings(_ input: InstallRequestParserInput) throws -> (request: InstallRequest, warnings: InstallRequestParseWarnings) {
        if input.upgrade && input.registerOnly {
            throw InstallRequestParseError.mutuallyExclusiveModes
        }
        let isFresh = !(input.upgrade || input.registerOnly)
        if isFresh {
            if input.piURL.isEmpty {
                throw InstallRequestParseError.piURLRequired
            }
            if input.pair.isEmpty {
                throw InstallRequestParseError.pairTokenRequired
            }
            if input.hostname.isEmpty {
                throw InstallRequestParseError.hostnameRequired
            }
        }
        let url: URL
        if !isFresh {
            // --upgrade / --register-only do not consume the URL — the
            // existing config.json supplies it. Use a placeholder
            // without parsing the operator's argument so they can pass
            // any value (or omit it) without spurious validation.
            url = URL(string: "https://localhost")!
        } else if input.piURL.isEmpty {
            // Fresh install required-fields are checked above; this
            // branch is unreachable on fresh install but keeps the
            // type non-optional.
            url = URL(string: "https://localhost")!
        } else {
            guard let parsed = URL(string: input.piURL) else {
                throw InstallRequestParseError.malformedPiURL(raw: input.piURL)
            }
            // Reject file://, relative paths, missing host for fresh
            // installs.
            do {
                try ConfigStore.validatePiURL(parsed)
            } catch {
                throw InstallRequestParseError.invalidPiURL(reason: String(describing: error))
            }
            url = parsed
        }

        var warnings = InstallRequestParseWarnings()
        if isFresh,
           url.scheme?.lowercased() == "http",
           !isLoopbackHost(url.host ?? "") {
            warnings.plaintextHTTPNonLoopback = true
        }

        let request = InstallRequest(
            piURL: url,
            pairingToken: input.pair,
            hostname: input.hostname,
            upgrade: input.upgrade,
            registerOnly: input.registerOnly)
        return (request, warnings)
    }
}

/// Whether `host` is a loopback address. localhost + 127.0.0.0/8 +
/// ::1 are the loopback set; everything else (including 10/8,
/// 192.168/16 LAN ranges) is treated as routable for the plaintext-
/// HTTP warning's purposes — plaintext over a LAN still leaks the
/// key on the wire.
///
/// The 127/8 check matches only IPv4 literals (four numeric octets)
/// to avoid a false-negative on hostnames like `127.example.com`
/// that happen to start with `127.`.
private func isLoopbackHost(_ host: String) -> Bool {
    if host.isEmpty { return false }
    let lower = host.lowercased()
    if lower == "localhost" { return true }
    if lower == "::1" { return true }
    if isLoopbackIPv4Literal(lower) { return true }
    return false
}

private func isLoopbackIPv4Literal(_ host: String) -> Bool {
    let octets = host.split(separator: ".")
    guard octets.count == 4 else { return false }
    guard let first = UInt8(octets[0]), first == 127 else { return false }
    guard UInt8(octets[1]) != nil,
          UInt8(octets[2]) != nil,
          UInt8(octets[3]) != nil else { return false }
    return true
}
