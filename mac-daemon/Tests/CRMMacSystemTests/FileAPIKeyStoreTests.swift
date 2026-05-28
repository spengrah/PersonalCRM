import XCTest
@testable import CRMMacSystem
import CRMMacLifecycle

final class FileAPIKeyStoreTests: XCTestCase {
    private var tmpDir: URL!
    private var keyPath: String!
    private var store: FileAPIKeyStore!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tmpDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("FileAPIKeyStoreTests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(
            at: tmpDir, withIntermediateDirectories: true)
        keyPath = tmpDir.appendingPathComponent("api-key").path
        store = FileAPIKeyStore(path: keyPath)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tmpDir)
        try super.tearDownWithError()
    }

    func testReadMissingFileThrowsNotFound() {
        XCTAssertThrowsError(try store.readAPIKey()) { error in
            guard let kErr = error as? KeychainStoreError, kErr == .notFound else {
                XCTFail("expected .notFound, got \(error)")
                return
            }
        }
    }

    func testWriteReadRoundtrip() throws {
        try store.writeAPIKey("s3cret-value")
        XCTAssertEqual(try store.readAPIKey(), "s3cret-value")
    }

    func testWriteCreatesFileWith0600Perms() throws {
        try store.writeAPIKey("abc")
        let attrs = try FileManager.default.attributesOfItem(atPath: keyPath)
        let perms = (attrs[.posixPermissions] as? NSNumber)?.intValue
        XCTAssertEqual(perms, 0o600)
    }

    func testWriteIsIdempotentAndReplacesValue() throws {
        try store.writeAPIKey("first")
        try store.writeAPIKey("second")
        XCTAssertEqual(try store.readAPIKey(), "second")
    }

    func testWriteCreatesParentDirectoryIfAbsent() throws {
        let nested = tmpDir.appendingPathComponent("nested/dir/api-key").path
        let nestedStore = FileAPIKeyStore(path: nested)
        try nestedStore.writeAPIKey("nested-val")
        XCTAssertEqual(try nestedStore.readAPIKey(), "nested-val")
    }

    func testDeleteRemovesFile() throws {
        try store.writeAPIKey("to-be-deleted")
        try store.deleteAPIKey()
        XCTAssertFalse(FileManager.default.fileExists(atPath: keyPath))
    }

    func testDeleteIsIdempotentWhenAbsent() throws {
        try store.deleteAPIKey()
        try store.deleteAPIKey()
    }

    func testReadStripsTrailingNewline() throws {
        // Simulate `echo "key" > api-key` from a human shell.
        try "human-edit\n".data(using: .utf8)!
            .write(to: URL(fileURLWithPath: keyPath))
        XCTAssertEqual(try store.readAPIKey(), "human-edit")
    }

    func testReadPreservesEmbeddedWhitespace() throws {
        try store.writeAPIKey("has spaces and\ttabs")
        XCTAssertEqual(try store.readAPIKey(), "has spaces and\ttabs")
    }

    func testWriteDoesNotLeaveTempFileBehind() throws {
        try store.writeAPIKey("clean")
        let contents = try FileManager.default.contentsOfDirectory(atPath: tmpDir.path)
        XCTAssertEqual(contents.filter { $0.contains(".tmp.") }, [])
    }

    /// The atomic-rename guarantee: when writeAPIKey fails AFTER an
    /// existing key is on disk, the old key must remain intact.
    /// Critical for the Repairer's stranded-key recovery model — if
    /// the OLD plaintext is preserved on persist-failure, the daemon
    /// can keep running with the old key (briefly, until the operator
    /// recovers via the printed new plaintext or a fresh re-pair).
    /// We simulate the failure by making the parent directory
    /// read-only so the temp-file write fails with EACCES.
    func testWriteFailurePreservesExistingValue() throws {
        try store.writeAPIKey("original-key")
        XCTAssertEqual(try store.readAPIKey(), "original-key")

        // Make parent dir read-only so writeAPIKey's temp-file step
        // throws. The pre-existing api-key file remains readable.
        let originalAttrs = try FileManager.default.attributesOfItem(atPath: tmpDir.path)
        defer {
            // Restore perms so tearDown can delete the dir.
            try? FileManager.default.setAttributes(originalAttrs, ofItemAtPath: tmpDir.path)
        }
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o500], ofItemAtPath: tmpDir.path)

        XCTAssertThrowsError(try store.writeAPIKey("new-key")) { error in
            guard let kErr = error as? KeychainStoreError, case .other = kErr else {
                XCTFail("expected .other (write failure), got \(error)")
                return
            }
        }

        // Restore perms before reading (chmod 500 blocks both writes
        // AND temp-file inspection from the test runner on some
        // configs; restore now and assert).
        try FileManager.default.setAttributes(originalAttrs, ofItemAtPath: tmpDir.path)

        // The OLD key is still on disk + readable.
        XCTAssertEqual(try store.readAPIKey(), "original-key",
            "atomic-rename guarantee: original key preserved on write failure")
        // No stranded tmp files.
        let contents = try FileManager.default.contentsOfDirectory(atPath: tmpDir.path)
        XCTAssertEqual(contents.filter { $0.contains(".tmp.") }, [],
            "tmp files must be cleaned up after write failure")
    }
}
