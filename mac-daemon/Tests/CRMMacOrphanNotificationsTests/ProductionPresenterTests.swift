// TC-PROD-PRESENT1 — verifies the production
// UserNotificationCenterPresenter adapter actually enqueues a
// request into UNUserNotificationCenter.current(). Complements
// TC-SMOKE1, which only exercises the fake presenter.
//
// Skipped in CI (no notification-capable host) and when the user
// hasn't granted notification authorization to the test runner.
// Local dev runs cover this; CI relies on TC-SMOKE1 + the rest of
// the OrphanNotificationCenter suite (fake-presenter-driven).
import XCTest
import UserNotifications
@testable import CRMMacOrphanNotifications

final class ProductionPresenterTests: XCTestCase {

    private let testIDPrefix = "crm-mac-prod-presenter-test-"

    override func setUp() async throws {
        try await super.setUp()
        if ProcessInfo.processInfo.environment["CI"] == "true" {
            throw XCTSkip("skipping production-presenter test in CI (no notification-capable test host)")
        }
        // Best-effort cleanup of stale test artifacts before each
        // run — keeps the assertion below reliable across repeated
        // dev runs.
        let center = UNUserNotificationCenter.current()
        let pending = await center.pendingNotificationRequests()
        let staleIDs = pending.map(\.identifier).filter { $0.hasPrefix(testIDPrefix) }
        if !staleIDs.isEmpty {
            center.removePendingNotificationRequests(withIdentifiers: staleIDs)
            center.removeDeliveredNotifications(withIdentifiers: staleIDs)
        }
    }

    func testProductionPresenterEnqueuesRequestIntoUserNotificationCenter() async throws {
        let presenter = UserNotificationCenterPresenter()
        let granted = await presenter.requestAuthorization()
        // A fresh dev machine without prior authorization will skip
        // — that's expected. The skip message tells the user how to
        // grant.
        try XCTSkipUnless(granted,
            "user notifications not authorized — grant permission in System Settings → Notifications and re-run")

        let identifier = "\(testIDPrefix)\(UUID().uuidString)"
        let spec = NotificationRequestSpec(
            identifier: identifier,
            title: "Production presenter smoke",
            body: "Synthetic test session",
            userInfo: ["test": "true"],
            sound: false)

        try await presenter.add(spec)

        // Query the OS pending queue and verify our request landed
        // there — the literal "UNUserNotificationCenter has the
        // expected request queued" check.
        let center = UNUserNotificationCenter.current()
        let pending = await center.pendingNotificationRequests()
        let found = pending.first(where: { $0.identifier == identifier })
        XCTAssertNotNil(found,
            "request was not in UNUserNotificationCenter.pendingNotificationRequests after add")
        XCTAssertEqual(found?.content.title, "Production presenter smoke")
        XCTAssertEqual(found?.content.body, "Synthetic test session")

        // Cleanup.
        await presenter.removePending(withIdentifiers: [identifier])
        await presenter.removeDelivered(withIdentifiers: [identifier])
    }
}
