// KeychainStore is the protocol the lifecycle workflows use to read,
// write, and delete the daemon's API key. The production
// implementation in CRMMacSystem wraps SecItemAdd /
// SecItemCopyMatching / SecItemUpdate / SecItemDelete; tests can
// inject an in-memory fake.
//
// Production attribute set: service = "xyz.spengrah.crm-mac", account
// = the constant string "api-key" (NOT the host UUID — that would
// couple the API-key retrieval to a parsed config.json).
import Foundation

public enum KeychainStoreError: Error, Equatable, CustomStringConvertible {
    case notFound
    case accessDenied
    case unexpected(status: Int32)
    case other(String)

    public var description: String {
        switch self {
        case .notFound: return "keychain item not found"
        case .accessDenied: return "keychain access denied"
        case .unexpected(let status): return "keychain unexpected status \(status)"
        case .other(let m): return "keychain error: \(m)"
        }
    }
}

public protocol KeychainStore {
    /// Read the API key plaintext. Throws .notFound when no entry
    /// exists.
    func readAPIKey() throws -> String
    /// Set the API key plaintext, replacing any existing value
    /// (idempotent). Used by install and upgrade.
    func writeAPIKey(_ value: String) throws
    /// Delete the API key entry. Idempotent — returns without error
    /// when the entry was already absent.
    func deleteAPIKey() throws
}
