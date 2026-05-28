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
                                   sessionUUID: "deadbeef-1111-2222-3333-444455556666",
                                   sequence: 7),
            "orphan:deadbeef-1111-2222-3333-444455556666:7")
    }

    func testIdentifierForConflict() {
        XCTAssertEqual(
            notificationIdentifier(reason: "conflict",
                                   sessionUUID: "deadbeef-1111-2222-3333-444455556666",
                                   sequence: 42),
            "conflict:deadbeef-1111-2222-3333-444455556666:42")
    }

    func testIdentifierDistinctAcrossReasons() {
        let uuid = "deadbeef-1111-2222-3333-444455556666"
        // Critical invariant: a notification's identifier must
        // differ across reasons for the same session — otherwise
        // the OS replaces an orphan with a conflict (or vice
        // versa) silently.
        XCTAssertNotEqual(
            notificationIdentifier(reason: "orphan", sessionUUID: uuid, sequence: 1),
            notificationIdentifier(reason: "conflict", sessionUUID: uuid, sequence: 1))
    }

    func testIdentifierDistinctAcrossSequencesForSameReasonAndSession() {
        // Critical invariant for the reconcile-vs-consume race fix:
        // two different mutationSequence values for the same
        // (reason, sessionUUID) MUST produce distinct identifiers,
        // so a stale-remove targeting sequence N cannot collaterally
        // strip a freshly-raised notification at sequence N+1.
        let uuid = "deadbeef-1111-2222-3333-444455556666"
        XCTAssertNotEqual(
            notificationIdentifier(reason: "orphan", sessionUUID: uuid, sequence: 5),
            notificationIdentifier(reason: "orphan", sessionUUID: uuid, sequence: 6))
    }

    // MARK: - isLegacyNotificationIdentifier

    func testLegacyIdentifierDetectionForOrphan() {
        XCTAssertTrue(isLegacyNotificationIdentifier(
            "orphan:deadbeef-1111-2222-3333-444455556666"))
    }

    func testLegacyIdentifierDetectionForConflict() {
        XCTAssertTrue(isLegacyNotificationIdentifier(
            "conflict:deadbeef-1111-2222-3333-444455556666"))
    }

    func testVersionedIdentifierIsNotLegacy() {
        // The new scheme has three colon-separated components.
        XCTAssertFalse(isLegacyNotificationIdentifier(
            "orphan:deadbeef-1111-2222-3333-444455556666:1"))
        XCTAssertFalse(isLegacyNotificationIdentifier(
            "conflict:deadbeef-1111-2222-3333-444455556666:99"))
    }

    func testUnrelatedIdentifierIsNotLegacy() {
        // Other identifier prefixes (or arbitrary strings) must
        // never be treated as legacy orphan/conflict ids.
        XCTAssertFalse(isLegacyNotificationIdentifier(
            "crm-mac-prod-presenter-test-deadbeef"))
        XCTAssertFalse(isLegacyNotificationIdentifier("foo:bar"))
        XCTAssertFalse(isLegacyNotificationIdentifier(""))
        XCTAssertFalse(isLegacyNotificationIdentifier("orphan"))
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
