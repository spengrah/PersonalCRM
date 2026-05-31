// Coverage for AnarlogSessionMetadataLookup — the SessionMetadataLookup
// adapter that bridges CRMMacOrphanNotifications into the
// CRMMacAnarlogSource filesystem readers.
import XCTest
import CRMMacCore
import CRMMacOrphanNotifications
@testable import CRMMacAnarlogSource

final class AnarlogSessionMetadataLookupTests: XCTestCase {

    private let sessionUUID = "deadbeef-1111-2222-3333-444455556666"
    private let rootPath = "/tmp/anarlog-test-root"

    private var sessionsPath: String { rootPath + "/sessions" }
    private var sessionDir: String { sessionsPath + "/" + sessionUUID }
    private var metaPath: String { sessionDir + "/_meta.json" }

    private final class StubConfigSource: AnarlogConfigSource, @unchecked Sendable {
        let cfg: AnarlogConfig?
        init(_ cfg: AnarlogConfig?) { self.cfg = cfg }
        func load() throws -> AnarlogConfig? { cfg }
    }

    private final class FailingConfigSource: AnarlogConfigSource, @unchecked Sendable {
        func load() throws -> AnarlogConfig? {
            throw AnarlogFilesystemError.ioError("synthetic")
        }
    }

    private final class StubFS: AnarlogFilesystem, @unchecked Sendable {
        var files: [String: Data] = [:]
        var directories: Set<String> = []

        func exists(_ path: String) -> Bool {
            files[path] != nil || directories.contains(path)
        }
        func isDirectory(_ path: String) -> Bool { directories.contains(path) }
        func isReadableDirectory(_ path: String) -> Bool { directories.contains(path) }
        func listDirectory(_ dir: String) throws -> [String] { [] }
        func readFile(_ path: String) throws -> Data {
            guard let b = files[path] else {
                throw AnarlogFilesystemError.ioError("not found: \(path)")
            }
            return b
        }
        func mtime(_ path: String) -> Date? { nil }

        func putDir(_ path: String) { directories.insert(path) }
        func putFile(_ path: String, bytes: Data) {
            files[path] = bytes
            let parent = (path as NSString).deletingLastPathComponent
            directories.insert(parent)
        }
    }

    private func makeConfig(enabled: Bool = true) -> AnarlogConfig {
        AnarlogConfig(rootPath: rootPath,
                      humansEnabled: false,
                      sessionsEnabled: enabled)
    }

    private func makeFS() -> StubFS {
        let fs = StubFS()
        fs.putDir(rootPath)
        fs.putDir(sessionsPath)
        return fs
    }

    // MARK: - happy path

    func testReturnsMetadataWhenMetaPresent() async throws {
        let fs = makeFS()
        fs.putDir(sessionDir)
        let metaJSON = """
        {
            "id": "\(sessionUUID)",
            "title": "Synthetic Test Session",
            "created_at": "2026-05-27T14:00:00Z"
        }
        """
        fs.putFile(metaPath, bytes: Data(metaJSON.utf8))
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNotNil(result)
        XCTAssertEqual(result?.title, "Synthetic Test Session")
        XCTAssertNotNil(result?.createdAt)
        XCTAssertEqual(result?.sessionDirURL?.path, sessionDir)
    }

    func testReturnsNilWhenConfigDisabled() async throws {
        let fs = makeFS()
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig(enabled: false)),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
    }

    func testReturnsNilWhenConfigLoadThrows() async throws {
        let fs = makeFS()
        let lookup = AnarlogSessionMetadataLookup(
            configSource: FailingConfigSource(),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
    }

    func testReturnsNilWhenConfigNil() async throws {
        let fs = makeFS()
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(nil),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
    }

    func testReturnsNilWhenSessionsRootMissing() async throws {
        let fs = StubFS() // no directories at all
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
    }

    func testReturnsNilWhenSessionDirMissing() async throws {
        let fs = makeFS()
        // session dir not created
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
    }

    func testReturnsSessionDirWhenMetaMissing() async throws {
        // Session dir exists but _meta.json is missing — still
        // return the sessionDirURL as session metadata (title/time
        // stay nil).
        let fs = makeFS()
        fs.putDir(sessionDir)
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNotNil(result)
        XCTAssertNil(result?.title)
        XCTAssertNil(result?.createdAt)
        XCTAssertEqual(result?.sessionDirURL?.path, sessionDir)
    }

    func testReturnsSessionDirWhenMetaUnparseable() async throws {
        let fs = makeFS()
        fs.putDir(sessionDir)
        fs.putFile(metaPath, bytes: Data("not valid json".utf8))
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNotNil(result)
        XCTAssertNil(result?.title)
        XCTAssertEqual(result?.sessionDirURL?.path, sessionDir)
    }

    func testTitleEmptyMapsToNil() async throws {
        // _meta.json has title="" — the lookup normalizes empty to
        // nil so the notification renders "Untitled session".
        let fs = makeFS()
        fs.putDir(sessionDir)
        let metaJSON = """
        {"title": "", "created_at": "2026-05-27T14:00:00Z"}
        """
        fs.putFile(metaPath, bytes: Data(metaJSON.utf8))
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNotNil(result)
        XCTAssertNil(result?.title)
    }

    func testRejectsNonCanonicalUUID() async throws {
        // Uppercase or otherwise non-canonical UUIDs are rejected
        // outright — the filesystem uses lowercase directory names.
        let fs = makeFS()
        fs.putDir(sessionDir)
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            filesystem: fs)
        // The canonical-validator lowercases the input first, so
        // "DEADBEEF-..." still maps to the lowercase form — that's
        // the intended posture. Test with a clearly invalid shape.
        let result = await lookup.lookup(sessionUUID: "not-a-uuid")
        XCTAssertNil(result)
    }
}
