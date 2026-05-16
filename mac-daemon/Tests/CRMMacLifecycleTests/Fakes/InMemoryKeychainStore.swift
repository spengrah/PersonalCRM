import Foundation
@testable import CRMMacLifecycle

/// Capture-only keychain. Models the production semantics exactly:
/// missing entry throws .notFound; writeAPIKey is idempotent;
/// deleteAPIKey is idempotent.
public final class InMemoryKeychainStore: KeychainStore, @unchecked Sendable {
    private var stored: String?

    public init(initial: String? = nil) {
        self.stored = initial
    }

    public func readAPIKey() throws -> String {
        guard let stored else { throw KeychainStoreError.notFound }
        return stored
    }

    public func writeAPIKey(_ value: String) throws {
        stored = value
    }

    public func deleteAPIKey() throws {
        stored = nil
    }

    public var currentValue: String? { stored }
}
