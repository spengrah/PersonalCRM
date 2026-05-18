// FileAPIKeyStore stores the daemon's API key in a plain UTF-8 file
// at the configured path, with 0600 perms. The file lives under the
// daemon's config dir (sibling of config.json / state.json) so a
// single `rm -rf ~/Library/Application\ Support/crm-mac/` cleans up
// everything.
//
// Threat model: same as ~/.aws/credentials and ~/.config/gh/hosts.yml
// — anyone with read access to the user account can read the key.
// We trade macOS Keychain's encryption at rest for the elimination
// of the "allow this app to access?" dialog that hangs the daemon
// every time the binary's CDHash changes (which is every rebuild
// under ad-hoc codesign).
//
// Writes go through a temp file + rename so a crash mid-write can't
// leave a half-written key on disk.
import Foundation
import CRMMacLifecycle

public struct FileAPIKeyStore: KeychainStore {
    private let path: String
    private let fm = FileManager.default

    public init(path: String) {
        self.path = path
    }

    public func readAPIKey() throws -> String {
        guard fm.fileExists(atPath: path) else {
            throw KeychainStoreError.notFound
        }
        let url = URL(fileURLWithPath: path)
        let data: Data
        do {
            data = try Data(contentsOf: url)
        } catch {
            throw KeychainStoreError.other("read failed: \(error)")
        }
        guard let s = String(data: data, encoding: .utf8) else {
            throw KeychainStoreError.other("api-key file is not utf8")
        }
        // Trim a trailing newline if a human edited the file with
        // `echo > api-key`. Embedded whitespace stays intact.
        return s.hasSuffix("\n") ? String(s.dropLast()) : s
    }

    public func writeAPIKey(_ value: String) throws {
        let parentDir = (path as NSString).deletingLastPathComponent
        do {
            try fm.createDirectory(
                atPath: parentDir, withIntermediateDirectories: true)
        } catch {
            throw KeychainStoreError.other("mkdir parent: \(error)")
        }
        let tmpPath = "\(path).tmp.\(UUID().uuidString)"
        let data = Data(value.utf8)
        do {
            try data.write(to: URL(fileURLWithPath: tmpPath), options: .atomic)
        } catch {
            throw KeychainStoreError.other("write tmp: \(error)")
        }
        do {
            try fm.setAttributes(
                [.posixPermissions: 0o600], ofItemAtPath: tmpPath)
        } catch {
            try? fm.removeItem(atPath: tmpPath)
            throw KeychainStoreError.other("chmod tmp: \(error)")
        }
        do {
            if fm.fileExists(atPath: path) {
                _ = try fm.replaceItemAt(
                    URL(fileURLWithPath: path),
                    withItemAt: URL(fileURLWithPath: tmpPath))
            } else {
                try fm.moveItem(atPath: tmpPath, toPath: path)
            }
        } catch {
            try? fm.removeItem(atPath: tmpPath)
            throw KeychainStoreError.other("rename: \(error)")
        }
    }

    public func deleteAPIKey() throws {
        guard fm.fileExists(atPath: path) else { return }
        do {
            try fm.removeItem(atPath: path)
        } catch {
            throw KeychainStoreError.other("delete: \(error)")
        }
    }
}
