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

    /// The atomic-rename guarantee — write failure leaves the on-disk
    /// key unchanged and cleans up any temp file. This is a
    /// general-purpose property of FileAPIKeyStore; the Repairer's
    /// stranded-key recovery is the load-bearing consumer: when a
    /// rotate-key persist fails AFTER the server has already swapped
    /// the hash, the daemon must throw the new plaintext to the
    /// operator (so they can hand-write it or re-pair) rather than
    /// silently land in a state where the on-disk key is neither the
    /// old nor the new value. Exercises the temp-file-write step by
    /// making the parent directory read-only so the temp open fails
    /// with EACCES. See also
    /// testWriteFailureAtRenameStepPreservesExistingValue for the
    /// complementary case (failure inside replaceItemAt).
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
            "atomic-rename guarantee: original key preserved on temp-file write failure")
        // No stranded tmp files.
        let contents = try FileManager.default.contentsOfDirectory(atPath: tmpDir.path)
        XCTAssertEqual(contents.filter { $0.contains(".tmp.") }, [],
            "tmp files must be cleaned up after write failure")
    }

    /// Complementary to testWriteFailurePreservesExistingValue:
    /// exercises the replaceItemAt failure path specifically. Sets the
    /// `uchg` (user-immutable) flag on the existing api-key file so
    /// the rename in writeAPIKey throws after the temp file has
    /// already been created. Verifies the original key survives AND
    /// that the temp file is cleaned up by FileAPIKeyStore's catch
    /// block on the rename step.
    func testWriteFailureAtRenameStepPreservesExistingValue() throws {
        try store.writeAPIKey("original-key")
        XCTAssertEqual(try store.readAPIKey(), "original-key")

        // Lock the target file so replaceItemAt cannot replace it.
        // The temp-file step will succeed because the parent dir is
        // still writable, but the rename step will fail.
        try FileManager.default.setAttributes(
            [.immutable: NSNumber(value: true)], ofItemAtPath: keyPath)
        defer {
            // Always clear the immutable flag so tearDown can clean up.
            try? FileManager.default.setAttributes(
                [.immutable: NSNumber(value: false)], ofItemAtPath: keyPath)
        }

        XCTAssertThrowsError(try store.writeAPIKey("new-key")) { error in
            guard let kErr = error as? KeychainStoreError, case .other = kErr else {
                XCTFail("expected .other (rename failure), got \(error)")
                return
            }
        }

        // The OLD key is still on disk + readable (rename never
        // committed).
        XCTAssertEqual(try store.readAPIKey(), "original-key",
            "atomic-rename guarantee: original key preserved on rename-step failure")
        // No stranded tmp files (the FileAPIKeyStore catch block on
        // the rename path removes the tmp file on failure).
        let contents = try FileManager.default.contentsOfDirectory(atPath: tmpDir.path)
        XCTAssertEqual(contents.filter { $0.contains(".tmp.") }, [],
            "tmp files must be cleaned up after rename failure")
    }
}
