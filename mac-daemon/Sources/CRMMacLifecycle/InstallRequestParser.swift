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

public enum InstallRequestParser {
    public static func parse(_ input: InstallRequestParserInput) throws -> InstallRequest {
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
        if input.piURL.isEmpty {
            url = URL(string: "https://localhost")!
        } else {
            guard let parsed = URL(string: input.piURL) else {
                throw InstallRequestParseError.malformedPiURL(raw: input.piURL)
            }
            if isFresh {
                // Reject file://, relative paths, missing host. For
                // --upgrade / --register-only the URL is unused so we
                // tolerate any value the operator may have supplied
                // out of habit.
                do {
                    try ConfigStore.validatePiURL(parsed)
                } catch {
                    throw InstallRequestParseError.invalidPiURL(reason: String(describing: error))
                }
            }
            url = parsed
        }
        return InstallRequest(
            piURL: url,
            pairingToken: input.pair,
            hostname: input.hostname,
            upgrade: input.upgrade,
            registerOnly: input.registerOnly)
    }
}
