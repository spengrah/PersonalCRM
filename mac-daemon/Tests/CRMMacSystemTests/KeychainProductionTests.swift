import XCTest
import Security
@testable import CRMMacSystem
import CRMMacLifecycle

/// Production Keychain integration smoke tests.
///
/// Skipped in CI — sandboxed macOS runners have no usable login
/// keychain and would either fail (no Keychain available) or pollute
/// a hypothetical shared one. Run locally with `swift test` to verify
/// the SecItem* wiring against the developer's keychain.
///
/// Uses a per-run account suffix so repeated local runs don't collide
/// and so a real install on the same machine isn't disturbed.
final class KeychainProductionTests: XCTestCase {
    private let testAccount = "test-\(UUID().uuidString)"

    override func setUpWithError() throws {
        try super.setUpWithError()
        try XCTSkipIf(ProcessInfo.processInfo.environment["CI"] == "true",
                      "Keychain production tests skipped in CI")
    }

    override func tearDown() {
        // Best-effort cleanup of any leftover test entry.
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: ProductionKeychainStore.service,
            kSecAttrAccount as String: testAccount,
        ]
        SecItemDelete(query as CFDictionary)
        super.tearDown()
    }

    func testWriteReadDeleteRoundtrip() throws {
        try write("hello")
        XCTAssertEqual(try read(), "hello")
        try delete()
        XCTAssertThrowsError(try read()) { error in
            guard let kErr = error as? KeychainStoreError, kErr == .notFound else {
                XCTFail("expected notFound, got \(error)")
                return
            }
        }
    }

    func testWriteIsIdempotent() throws {
        try write("first")
        try write("second")
        XCTAssertEqual(try read(), "second")
    }

    func testDeleteIsIdempotent() throws {
        try delete()
        try delete()
    }

    // MARK: - local SecItem helpers with custom account

    private func write(_ value: String) throws {
        let data = Data(value.utf8)
        let base: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: ProductionKeychainStore.service,
            kSecAttrAccount as String: testAccount,
        ]
        let attrs: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updateStatus = SecItemUpdate(base as CFDictionary, attrs as CFDictionary)
        if updateStatus == errSecSuccess { return }
        guard updateStatus == errSecItemNotFound else {
            throw KeychainStoreError.unexpected(status: updateStatus)
        }
        var add = base
        for (k, v) in attrs { add[k] = v }
        let addStatus = SecItemAdd(add as CFDictionary, nil)
        guard addStatus == errSecSuccess else {
            throw KeychainStoreError.unexpected(status: addStatus)
        }
    }

    private func read() throws -> String {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: ProductionKeychainStore.service,
            kSecAttrAccount as String: testAccount,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var result: AnyObject?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        switch status {
        case errSecSuccess:
            guard let data = result as? Data, let s = String(data: data, encoding: .utf8) else {
                throw KeychainStoreError.other("not utf8")
            }
            return s
        case errSecItemNotFound:
            throw KeychainStoreError.notFound
        default:
            throw KeychainStoreError.unexpected(status: status)
        }
    }

    private func delete() throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: ProductionKeychainStore.service,
            kSecAttrAccount as String: testAccount,
        ]
        let status = SecItemDelete(query as CFDictionary)
        switch status {
        case errSecSuccess, errSecItemNotFound:
            return
        default:
            throw KeychainStoreError.unexpected(status: status)
        }
    }
}
