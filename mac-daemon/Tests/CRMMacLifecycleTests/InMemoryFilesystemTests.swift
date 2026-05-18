// InMemoryFilesystemTests exercise the in-memory FilesystemAdapter
// fake's directory-rename support (plan D14). The InMemoryFilesystem
// is now used by tests covering the upgrade-path bundle swap (D8) and
// by BundleAssemblerTests; both rely on `rename` working on
// directories, not just files.
import XCTest
@testable import CRMMacLifecycle

final class InMemoryFilesystemTests: XCTestCase {

    func testRenameDirectoryMovesAllChildren() throws {
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: "/a")
        try fs.write(Data("x".utf8), to: "/a/x")
        try fs.createDirectory(at: "/a/sub")
        try fs.write(Data("y".utf8), to: "/a/sub/y")

        try fs.rename(from: "/a", to: "/b")

        XCTAssertTrue(fs.fileExists(at: "/b"))
        XCTAssertTrue(fs.fileExists(at: "/b/x"))
        XCTAssertTrue(fs.fileExists(at: "/b/sub"))
        XCTAssertTrue(fs.fileExists(at: "/b/sub/y"))
        XCTAssertFalse(fs.fileExists(at: "/a"))
        XCTAssertFalse(fs.fileExists(at: "/a/x"))
        XCTAssertFalse(fs.fileExists(at: "/a/sub"))
        XCTAssertFalse(fs.fileExists(at: "/a/sub/y"))
    }

    func testRenameDirectoryMovesNestedSubdirs() throws {
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: "/root/Contents/Library/LaunchAgents")
        try fs.write(Data("info".utf8), to: "/root/Contents/Info.plist")
        try fs.write(Data("macho".utf8), to: "/root/Contents/MacOS/crm-mac")
        try fs.createDirectory(at: "/root/Contents/MacOS")
        try fs.write(Data("plist".utf8), to: "/root/Contents/Library/LaunchAgents/xyz.plist")

        try fs.rename(from: "/root", to: "/installed")

        XCTAssertTrue(fs.fileExists(at: "/installed/Contents/Info.plist"))
        XCTAssertTrue(fs.fileExists(at: "/installed/Contents/MacOS/crm-mac"))
        XCTAssertTrue(fs.fileExists(at: "/installed/Contents/Library/LaunchAgents/xyz.plist"))
        XCTAssertTrue(fs.fileExists(at: "/installed/Contents/Library/LaunchAgents"))
        XCTAssertTrue(fs.fileExists(at: "/installed/Contents/MacOS"))
        XCTAssertFalse(fs.fileExists(at: "/root"))
        XCTAssertFalse(fs.fileExists(at: "/root/Contents/Info.plist"))
    }

    func testRenameDirectoryToExistingPathFailsLikeENOTEMPTY() throws {
        // The fake refuses to overwrite a non-empty destination
        // directory — mirrors POSIX rename(2) ENOTEMPTY. This catches
        // a class of production mistakes where the installer
        // accidentally calls rename with a non-empty destination
        // instead of using backup-rename-then-swap (plan D8 U3-U5).
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: "/src")
        try fs.write(Data("a".utf8), to: "/src/a")
        try fs.createDirectory(at: "/dst")
        try fs.write(Data("b".utf8), to: "/dst/b")

        XCTAssertThrowsError(try fs.rename(from: "/src", to: "/dst")) { error in
            guard let e = error as? FilesystemError else {
                XCTFail("expected FilesystemError, got \(error)")
                return
            }
            if case .ioError(let m) = e {
                XCTAssertTrue(m.contains("not empty"),
                    "expected 'not empty' in error, got '\(m)'")
            } else {
                XCTFail("expected ioError, got \(e)")
            }
        }
        // Source unmoved.
        XCTAssertTrue(fs.fileExists(at: "/src/a"))
        XCTAssertTrue(fs.fileExists(at: "/dst/b"))
    }

    func testRenameDirectoryToEmptyExistingPathSucceeds() throws {
        // An empty destination dir is fine — same behavior as
        // FileManager.moveItem on APFS.
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: "/src")
        try fs.write(Data("a".utf8), to: "/src/a")
        try fs.createDirectory(at: "/dst")  // empty

        try fs.rename(from: "/src", to: "/dst")
        XCTAssertTrue(fs.fileExists(at: "/dst/a"))
        XCTAssertFalse(fs.fileExists(at: "/src/a"))
    }

    func testRenameMissingSourceThrowsNotFound() {
        let fs = InMemoryFilesystem()
        XCTAssertThrowsError(try fs.rename(from: "/nope", to: "/elsewhere")) { error in
            guard let e = error as? FilesystemError, case .notFound = e else {
                XCTFail("expected notFound, got \(error)")
                return
            }
        }
    }

    func testRemoveDirectoryDropsAllDescendants() throws {
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: "/a/b/c")
        try fs.write(Data("x".utf8), to: "/a/file")
        try fs.write(Data("y".utf8), to: "/a/b/c/leaf")

        try fs.remove(at: "/a")
        XCTAssertFalse(fs.fileExists(at: "/a"))
        XCTAssertFalse(fs.fileExists(at: "/a/file"))
        XCTAssertFalse(fs.fileExists(at: "/a/b"))
        XCTAssertFalse(fs.fileExists(at: "/a/b/c"))
        XCTAssertFalse(fs.fileExists(at: "/a/b/c/leaf"))
    }
}
