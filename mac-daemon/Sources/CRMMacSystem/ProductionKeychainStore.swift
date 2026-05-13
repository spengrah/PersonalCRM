// ProductionKeychainStore wraps Security framework SecItem* APIs to
// store the daemon's API key in the user's Keychain.
//
// Attribute set:
//   - service = "xyz.spengrah.crm-mac"
//   - account = constant "api-key" (NOT the host UUID — so the key can
//     be retrieved even when config.json is corrupted)
//   - kSecAttrAccessible = AfterFirstUnlockThisDeviceOnly (the
//     standard option for non-interactive launchd-spawned agents)
//   - kSecAttrSynchronizable = false (do NOT iCloud-sync the key)
//   - kSecAttrLabel = "Personal CRM Mac daemon API key"
import Foundation
import Security
import CRMMacLifecycle

public struct ProductionKeychainStore: KeychainStore {
    public static let service: String = "xyz.spengrah.crm-mac"
    public static let account: String = "api-key"
    public static let label: String = "Personal CRM Mac daemon API key"

    public init() {}

    public func readAPIKey() throws -> String {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: Self.account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var result: AnyObject?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        switch status {
        case errSecSuccess:
            guard let data = result as? Data, let s = String(data: data, encoding: .utf8) else {
                throw KeychainStoreError.other("api-key not utf8 decodable")
            }
            return s
        case errSecItemNotFound:
            throw KeychainStoreError.notFound
        case errSecInteractionNotAllowed, errSecAuthFailed:
            throw KeychainStoreError.accessDenied
        default:
            throw KeychainStoreError.unexpected(status: status)
        }
    }

    public func writeAPIKey(_ value: String) throws {
        let valueData = Data(value.utf8)
        let baseQuery: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: Self.account,
        ]
        // Try update first; on errSecItemNotFound fall through to add.
        let attributesToUpdate: [String: Any] = [
            kSecValueData as String: valueData,
            kSecAttrLabel as String: Self.label,
            kSecAttrSynchronizable as String: false,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updateStatus = SecItemUpdate(baseQuery as CFDictionary, attributesToUpdate as CFDictionary)
        switch updateStatus {
        case errSecSuccess:
            return
        case errSecItemNotFound:
            break  // Fall through to add.
        case errSecInteractionNotAllowed, errSecAuthFailed:
            throw KeychainStoreError.accessDenied
        default:
            throw KeychainStoreError.unexpected(status: updateStatus)
        }

        var addQuery = baseQuery
        addQuery[kSecValueData as String] = valueData
        addQuery[kSecAttrLabel as String] = Self.label
        addQuery[kSecAttrSynchronizable as String] = false
        addQuery[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly

        let addStatus = SecItemAdd(addQuery as CFDictionary, nil)
        switch addStatus {
        case errSecSuccess:
            return
        case errSecDuplicateItem:
            // Concurrent writer added the item between our update-
            // returning-notFound and our add. Retry as an update; if
            // that still fails, surface whatever the system tells us.
            let retryStatus = SecItemUpdate(baseQuery as CFDictionary, attributesToUpdate as CFDictionary)
            switch retryStatus {
            case errSecSuccess:
                return
            case errSecInteractionNotAllowed, errSecAuthFailed:
                throw KeychainStoreError.accessDenied
            default:
                throw KeychainStoreError.unexpected(status: retryStatus)
            }
        case errSecInteractionNotAllowed, errSecAuthFailed:
            throw KeychainStoreError.accessDenied
        default:
            throw KeychainStoreError.unexpected(status: addStatus)
        }
    }

    public func deleteAPIKey() throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: Self.account,
        ]
        let status = SecItemDelete(query as CFDictionary)
        switch status {
        case errSecSuccess, errSecItemNotFound:
            return
        case errSecInteractionNotAllowed, errSecAuthFailed:
            throw KeychainStoreError.accessDenied
        default:
            throw KeychainStoreError.unexpected(status: status)
        }
    }
}
