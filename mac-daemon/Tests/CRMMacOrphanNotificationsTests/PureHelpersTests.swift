// Coverage for the pure helpers in CRMMacOrphanNotifications:
// LinkageStateMapping, notificationIdentifier, clickTargetURL.
import XCTest
@testable import CRMMacOrphanNotifications

final class PureHelpersTests: XCTestCase {

    // MARK: - mapLinkageStateToReason

    func testMapsConflictPendingToConflict() {
        XCTAssertEqual(mapLinkageStateToReason("conflict_pending"), "conflict")
    }

    func testMapsOrphanNeedsReviewToOrphan() {
        XCTAssertEqual(mapLinkageStateToReason("orphan_needs_review"), "orphan")
    }

    func testReturnsNilForUnknownLinkageState() {
        XCTAssertNil(mapLinkageStateToReason("future_unknown_state"))
        XCTAssertNil(mapLinkageStateToReason(""))
        XCTAssertNil(mapLinkageStateToReason("conflict"))   // Already-mapped reason, not a linkage_state.
        XCTAssertNil(mapLinkageStateToReason("orphan"))
    }

    // MARK: - notificationIdentifier

    func testIdentifierForOrphan() {
        XCTAssertEqual(
            notificationIdentifier(reason: "orphan",
                                   sessionUUID: "deadbeef-1111-2222-3333-444455556666"),
            "orphan:deadbeef-1111-2222-3333-444455556666")
    }

    func testIdentifierForConflict() {
        XCTAssertEqual(
            notificationIdentifier(reason: "conflict",
                                   sessionUUID: "deadbeef-1111-2222-3333-444455556666"),
            "conflict:deadbeef-1111-2222-3333-444455556666")
    }

    func testIdentifierDistinctAcrossReasons() {
        let uuid = "deadbeef-1111-2222-3333-444455556666"
        // Critical invariant: a notification's identifier must
        // differ across reasons for the same session — otherwise
        // the OS replaces an orphan with a conflict (or vice
        // versa) silently.
        XCTAssertNotEqual(
            notificationIdentifier(reason: "orphan", sessionUUID: uuid),
            notificationIdentifier(reason: "conflict", sessionUUID: uuid))
    }

    // MARK: - clickTargetURL

    func testOrphanReturnsSessionDirectoryURL() {
        let dir = URL(fileURLWithPath: "/tmp/anarlog/sessions/deadbeef-1111-2222-3333-444455556666")
        let metadata = SessionMetadata(title: "X", createdAt: nil, sessionDirURL: dir)
        let url = clickTargetURL(
            reason: "orphan",
            sessionUUID: "deadbeef-1111-2222-3333-444455556666",
            metadata: metadata,
            piURL: URL(string: "https://pi.example")!)
        XCTAssertEqual(url, dir)
    }

    func testOrphanReturnsNilWhenMetadataNil() {
        XCTAssertNil(clickTargetURL(
            reason: "orphan",
            sessionUUID: "deadbeef-1111-2222-3333-444455556666",
            metadata: nil,
            piURL: URL(string: "https://pi.example")!))
    }

    func testOrphanReturnsNilWhenMetadataLacksSessionDir() {
        let metadata = SessionMetadata(title: "X", createdAt: nil, sessionDirURL: nil)
        XCTAssertNil(clickTargetURL(
            reason: "orphan",
            sessionUUID: "deadbeef-1111-2222-3333-444455556666",
            metadata: metadata,
            piURL: URL(string: "https://pi.example")!))
    }

    func testConflictReturnsPiUIDeepLink() {
        let url = clickTargetURL(
            reason: "conflict",
            sessionUUID: "deadbeef-1111-2222-3333-444455556666",
            metadata: nil,
            piURL: URL(string: "https://pi.example")!)
        XCTAssertNotNil(url)
        let comps = URLComponents(url: url!, resolvingAgainstBaseURL: false)!
        XCTAssertEqual(comps.path, "/imports")
        let q = comps.queryItems ?? []
        let tab = q.first(where: { $0.name == "tab" })?.value
        let session = q.first(where: { $0.name == "session" })?.value
        XCTAssertEqual(tab, "needs-attention")
        XCTAssertEqual(session, "deadbeef-1111-2222-3333-444455556666")
    }

    func testConflictHandlesPiURLWithTrailingSlash() {
        let url = clickTargetURL(
            reason: "conflict",
            sessionUUID: "deadbeef-1111-2222-3333-444455556666",
            metadata: nil,
            piURL: URL(string: "https://pi.example/")!)
        XCTAssertNotNil(url)
        XCTAssertEqual(url?.path, "/imports")
    }

    func testUnknownReasonReturnsNil() {
        XCTAssertNil(clickTargetURL(
            reason: "future_unknown",
            sessionUUID: "x",
            metadata: nil,
            piURL: URL(string: "https://pi.example")!))
    }
}
