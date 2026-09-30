import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacOrphanNotifications

fileprivate final class RecordingLogger: LoggerProtocol, @unchecked Sendable {
    private let lock = NSLock()
    private var storedEntries: [(LogLevel, String, [String: LogValue])] = []

    func log(_ level: LogLevel, _ message: String, metadata: [String: LogValue]) {
        lock.lock()
        defer { lock.unlock() }
        storedEntries.append((level, message, metadata))
    }

    var entries: [(LogLevel, String, [String: LogValue])] {
        lock.lock()
        defer { lock.unlock() }
        return storedEntries
    }
}

final class AnarlogHealthNotifierTests: XCTestCase {
    private static let now = Date(timeIntervalSince1970: 1_780_000_000)
    private static let brokenIdentifiers = [
        "anarlog_source:broken:binary_not_found",
        "anarlog_source:broken:unsupported_schema_version",
        "anarlog_source:broken:missing_field",
        "anarlog_source:broken:malformed_output",
        "anarlog_source:broken:operator_person_id_unset",
        "anarlog_source:broken:database_not_found",
        "anarlog_source:broken:nonzero_exit",
        "anarlog_source:broken:timeout",
        "anarlog_source:broken:deletions_withheld",
    ]
    private static let brokenTitle = "Anarlog sync is broken"
    private static let quietTitle = "No new Anarlog sessions"
    private static let quietBody = "Anarlog lists no session created in the last 14 days."

    private var recent: Date { Self.now.addingTimeInterval(-86_400) }
    private var stale: Date { Self.now.addingTimeInterval(-15 * 86_400) }

    private func makeRig(
        authorizationResult: Bool = true,
        addError: Error? = nil
    ) -> (presenter: FakeUserNotificationPresenter, notifier: AnarlogHealthNotifier) {
        let presenter = FakeUserNotificationPresenter(
            authorizationResult: authorizationResult,
            addError: addError)
        let notifier = AnarlogHealthNotifier(
            presenter: presenter,
            logger: NoopLogger(),
            clock: { Self.now })
        return (presenter, notifier)
    }

    private func record(
        _ outcome: AnarlogTickOutcome,
        from source: SourceID = .anarlogSessions,
        using notifier: AnarlogHealthNotifier
    ) async {
        await notifier.record(source: source, outcome: outcome)
    }

    private func addCalls(_ presenter: FakeUserNotificationPresenter) async -> [NotificationRequestSpec] {
        await presenter.recordedAddCalls()
    }

    private func addIdentifiers(_ presenter: FakeUserNotificationPresenter) async -> [String] {
        let calls = await addCalls(presenter)
        return calls.map(\.identifier)
    }

    private func removalCalls(_ presenter: FakeUserNotificationPresenter) async -> [[String]] {
        await presenter.recordedRemoveDelivered()
    }

    private func assertRemovalCallsEqual(
        _ presenter: FakeUserNotificationPresenter,
        file: StaticString = #filePath,
        line: UInt = #line
    ) async {
        let delivered = await presenter.recordedRemoveDelivered()
        let pending = await presenter.recordedRemovePending()
        XCTAssertEqual(delivered, pending, file: file, line: line)
    }

    private func assertSingleAdd(
        _ presenter: FakeUserNotificationPresenter,
        identifier: String,
        title: String,
        body: String,
        file: StaticString = #filePath,
        line: UInt = #line
    ) async {
        let calls = await addCalls(presenter)
        XCTAssertEqual(calls.count, 1, file: file, line: line)
        guard let call = calls.first else { return }
        XCTAssertEqual(call.identifier, identifier, file: file, line: line)
        XCTAssertEqual(call.title, title, file: file, line: line)
        XCTAssertEqual(call.body, body, file: file, line: line)
        XCTAssertEqual(call.userInfo, [:], file: file, line: line)
        XCTAssertTrue(call.sound, file: file, line: line)
    }

    // MAC-052: stable identifiers and exhaustive failure identity mapping.
    func testIdentifierTableAndFailureMapping() {
        XCTAssertEqual(
            AnarlogBrokenCondition.allCases.map(\.notificationIdentifier),
            Self.brokenIdentifiers)
        XCTAssertEqual(AnarlogHealthNotifier.quietIdentifier, "anarlog_source:quiet")

        let mappings: [(AnarlogCLIFailure, String)] = [
            (.binaryNotFound, "binary_not_found"),
            (.unsupportedSchemaVersion("2"), "unsupported_schema_version"),
            (.missingField(command: "meetings get", field: "data.title"), "missing_field"),
            (.malformedOutput(command: "meetings list"), "malformed_output"),
            (.operatorPersonIDUnset, "operator_person_id_unset"),
            (.databaseNotFound, "database_not_found"),
            (.nonZeroExit(code: 5, errorCode: "internal"), "nonzero_exit"),
            (.timeout, "timeout"),
        ]
        for (failure, rawValue) in mappings {
            XCTAssertEqual(AnarlogBrokenCondition(failure).rawValue, rawValue)
        }
    }

    // MAC-051 and MAC-052: each permanent failure raises on its first report.
    func testPermanentFailuresRaiseExactNotifications() async {
        let cases: [(AnarlogCLIFailure, String, String)] = [
            (
                .binaryNotFound,
                Self.brokenIdentifiers[0],
                "The anarlog CLI was not found. Set its path with: crm-mac configure anarlog --cli-path"
            ),
            (
                .unsupportedSchemaVersion("2"),
                Self.brokenIdentifiers[1],
                "The anarlog CLI reports contract version \"2\"; crm-mac supports \"1\"."
            ),
            (
                .missingField(command: "meetings get", field: "data.title"),
                Self.brokenIdentifiers[2],
                "The anarlog CLI output for meetings get lacks the field data.title."
            ),
            (
                .malformedOutput(command: "meetings list"),
                Self.brokenIdentifiers[3],
                "The anarlog CLI returned unreadable output for meetings list."
            ),
            (
                .operatorPersonIDUnset,
                Self.brokenIdentifiers[4],
                "Your Anarlog person ID is not set. Set it with: crm-mac configure anarlog --operator-person-id"
            ),
        ]

        for (failure, identifier, body) in cases {
            let rig = makeRig()
            await record(.failed(failure), using: rig.notifier)
            await assertSingleAdd(
                rig.presenter,
                identifier: identifier,
                title: Self.brokenTitle,
                body: body)
        }
    }

    // MAC-052: runtime failures raise after two consecutive reports.
    func testRuntimeFailuresRaiseAfterTwoReports() async {
        let cases: [(AnarlogCLIFailure, String, String)] = [
            (.databaseNotFound, Self.brokenIdentifiers[5], "The anarlog CLI cannot find the Anarlog database."),
            (
                .nonZeroExit(code: 5, errorCode: "internal"),
                Self.brokenIdentifiers[6],
                "The anarlog CLI exited with code 5 (internal)."
            ),
            (.timeout, Self.brokenIdentifiers[7], "The anarlog CLI did not answer in time."),
        ]

        for (failure, identifier, body) in cases {
            let rig = makeRig()
            await record(.failed(failure), using: rig.notifier)
            var calls = await addCalls(rig.presenter)
            XCTAssertTrue(calls.isEmpty)
            await record(.failed(failure), using: rig.notifier)
            calls = await addCalls(rig.presenter)
            XCTAssertEqual(calls.count, 1)
            XCTAssertEqual(calls.first?.identifier, identifier)
            XCTAssertEqual(calls.first?.title, Self.brokenTitle)
            XCTAssertEqual(calls.first?.body, body)
            XCTAssertEqual(calls.first?.userInfo, [:])
            XCTAssertEqual(calls.first?.sound, true)
        }

        let nilErrorRig = makeRig()
        let nilErrorFailure = AnarlogCLIFailure.nonZeroExit(code: 1, errorCode: nil)
        await record(.failed(nilErrorFailure), using: nilErrorRig.notifier)
        await record(.failed(nilErrorFailure), using: nilErrorRig.notifier)
        await assertSingleAdd(
            nilErrorRig.presenter,
            identifier: Self.brokenIdentifiers[6],
            title: Self.brokenTitle,
            body: "The anarlog CLI exited with code 1.")
    }

    // MAC-052: a clean tick resets a one-tick runtime streak.
    func testTransientFailureFollowedByCleanDoesNotRaise() async {
        let rig = makeRig()
        await record(.failed(.timeout), using: rig.notifier)
        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        await record(.failed(.timeout), using: rig.notifier)
        let calls = await addCalls(rig.presenter)
        XCTAssertTrue(calls.isEmpty)
    }

    // MAC-052: runtime streaks are independent for each source.
    func testRuntimeFailureStreakIsPerSource() async {
        let rig = makeRig()
        await record(.failed(.timeout), using: rig.notifier)
        await record(.failed(.timeout), from: .anarlogHumans, using: rig.notifier)
        var calls = await addCalls(rig.presenter)
        XCTAssertTrue(calls.isEmpty)

        await record(.failed(.timeout), using: rig.notifier)
        calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[7]])
    }

    // MAC-052: a permanent failure resets the source's runtime streak.
    func testPermanentFailureBreaksRuntimeStreak() async {
        let rig = makeRig()
        await record(.failed(.timeout), using: rig.notifier)
        await record(.failed(.binaryNotFound), using: rig.notifier)
        await record(.failed(.timeout), using: rig.notifier)
        let calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[0]])
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[0]])
        await assertRemovalCallsEqual(rig.presenter)
    }

    // MAC-052: consecutive runtime failures may change case and hold the latest case.
    func testRuntimeFailureCaseSwitchContinuesStreak() async {
        let rig = makeRig()
        await record(.failed(.timeout), using: rig.notifier)
        await record(.failed(.timeout), using: rig.notifier)
        await record(.failed(.databaseNotFound), using: rig.notifier)
        let calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[7], Self.brokenIdentifiers[5]])
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[7]])
        await assertRemovalCallsEqual(rig.presenter)
    }

    // MAC-052: changed detail does not re-alert an already shown condition.
    func testShownConditionDoesNotRealertWhenDetailChanges() async {
        let withheldRig = makeRig()
        await record(.deletionsWithheld(withheld: 3, newestSessionCreatedAt: recent), using: withheldRig.notifier)
        await record(.deletionsWithheld(withheld: 7, newestSessionCreatedAt: recent), using: withheldRig.notifier)
        await assertSingleAdd(
            withheldRig.presenter,
            identifier: Self.brokenIdentifiers[8],
            title: Self.brokenTitle,
            body: "3 Anarlog session deletions were withheld because they exceed the deletion cap. To apply them, raise it with: crm-mac configure anarlog --deletion-cap")

        let versionRig = makeRig()
        await record(.failed(.unsupportedSchemaVersion("2")), using: versionRig.notifier)
        await record(.failed(.unsupportedSchemaVersion("3")), using: versionRig.notifier)
        await assertSingleAdd(
            versionRig.presenter,
            identifier: Self.brokenIdentifiers[1],
            title: Self.brokenTitle,
            body: "The anarlog CLI reports contract version \"2\"; crm-mac supports \"1\".")

        let runtimeRig = makeRig()
        await record(.failed(.nonZeroExit(code: 1, errorCode: "a")), using: runtimeRig.notifier)
        await record(.failed(.nonZeroExit(code: 1, errorCode: "a")), using: runtimeRig.notifier)
        await record(.failed(.nonZeroExit(code: 5, errorCode: "b")), using: runtimeRig.notifier)
        await assertSingleAdd(
            runtimeRig.presenter,
            identifier: Self.brokenIdentifiers[6],
            title: Self.brokenTitle,
            body: "The anarlog CLI exited with code 1 (a).")
    }

    // MAC-049: a clean listing clears the withheld-deletions condition.
    func testWithheldDeletionsClearOnCleanListing() async {
        let rig = makeRig()
        await record(.deletionsWithheld(withheld: 6, newestSessionCreatedAt: recent), using: rig.notifier)
        await assertSingleAdd(
            rig.presenter,
            identifier: Self.brokenIdentifiers[8],
            title: Self.brokenTitle,
            body: "6 Anarlog session deletions were withheld because they exceed the deletion cap. To apply them, raise it with: crm-mac configure anarlog --deletion-cap")
        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[8]])
        await assertRemovalCallsEqual(rig.presenter)
    }

    // I15: independently held conditions clear independently.
    func testCoexistingConditionsClearIndependently() async {
        let rig = makeRig()
        await record(.deletionsWithheld(withheld: 6, newestSessionCreatedAt: recent), using: rig.notifier)
        await record(.failed(.binaryNotFound), from: .anarlogHumans, using: rig.notifier)
        var calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[8], Self.brokenIdentifiers[0]])
        let removeCountAfterBinary = await removalCalls(rig.presenter).count

        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        var removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[8]])
        XCTAssertEqual(removals.dropFirst(removeCountAfterBinary).flatMap { $0 }.contains(Self.brokenIdentifiers[0]), false)

        await record(.clean(newestSessionCreatedAt: recent), from: .anarlogHumans, using: rig.notifier)
        removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[0]])
        calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.count, 2)
        await assertRemovalCallsEqual(rig.presenter)
    }

    // MAC-052: different sources may hold different conditions.
    func testDifferentConditionsCanBeHeldByDifferentSources() async {
        let rig = makeRig()
        await record(.failed(.binaryNotFound), using: rig.notifier)
        let removalCountAfterBinary = await removalCalls(rig.presenter).count
        await record(.failed(.operatorPersonIDUnset), from: .anarlogHumans, using: rig.notifier)
        let calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[0], Self.brokenIdentifiers[4]])

        await record(.clean(newestSessionCreatedAt: recent), from: .anarlogHumans, using: rig.notifier)
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[4], AnarlogHealthNotifier.quietIdentifier])
        XCTAssertFalse(removals.dropFirst(removalCountAfterBinary).flatMap { $0 }.contains(Self.brokenIdentifiers[0]))
        await assertRemovalCallsEqual(rig.presenter)
    }

    // I15: a condition identifier is shared across sources until no source holds it.
    func testConditionIsNotKeyedBySource() async {
        let rig = makeRig()
        await record(.failed(.binaryNotFound), using: rig.notifier)
        await record(.failed(.binaryNotFound), from: .anarlogHumans, using: rig.notifier)
        var calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[0]])

        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        var removals = await removalCalls(rig.presenter)
        XCTAssertFalse(removals.flatMap { $0 }.contains(Self.brokenIdentifiers[0]))
        await record(.clean(newestSessionCreatedAt: recent), from: .anarlogHumans, using: rig.notifier)
        removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[0]])
        calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.count, 1)
        await assertRemovalCallsEqual(rig.presenter)
    }

    // MAC-053: empty or stale listings raise quiet; recent listings clear it.
    func testQuietConditionUsesFourteenDayWindowFromEitherSource() async {
        let emptyRig = makeRig()
        await record(.clean(newestSessionCreatedAt: nil), using: emptyRig.notifier)
        await assertSingleAdd(
            emptyRig.presenter,
            identifier: AnarlogHealthNotifier.quietIdentifier,
            title: Self.quietTitle,
            body: Self.quietBody)

        let staleRig = makeRig()
        await record(.clean(newestSessionCreatedAt: stale), using: staleRig.notifier)
        await assertSingleAdd(
            staleRig.presenter,
            identifier: AnarlogHealthNotifier.quietIdentifier,
            title: Self.quietTitle,
            body: Self.quietBody)

        let recentRig = makeRig()
        await record(.clean(newestSessionCreatedAt: recent), using: recentRig.notifier)
        let recentCalls = await addCalls(recentRig.presenter)
        XCTAssertTrue(recentCalls.isEmpty)

        let clearsRig = makeRig()
        await record(.clean(newestSessionCreatedAt: nil), using: clearsRig.notifier)
        await record(.clean(newestSessionCreatedAt: recent), using: clearsRig.notifier)
        let removals = await removalCalls(clearsRig.presenter)
        XCTAssertEqual(removals.last, [AnarlogHealthNotifier.quietIdentifier])
        await assertRemovalCallsEqual(clearsRig.presenter)

        let exactWindowRig = makeRig()
        await record(
            .clean(newestSessionCreatedAt: Self.now.addingTimeInterval(-AnarlogHealthNotifier.quietWindow)),
            using: exactWindowRig.notifier)
        let exactCalls = await addCalls(exactWindowRig.presenter)
        XCTAssertTrue(exactCalls.isEmpty)

        let overWindowRig = makeRig()
        await record(
            .clean(newestSessionCreatedAt: Self.now.addingTimeInterval(-AnarlogHealthNotifier.quietWindow - 1)),
            using: overWindowRig.notifier)
        await assertSingleAdd(
            overWindowRig.presenter,
            identifier: AnarlogHealthNotifier.quietIdentifier,
            title: Self.quietTitle,
            body: Self.quietBody)

        let humansEmptyRig = makeRig()
        await record(.clean(newestSessionCreatedAt: nil), from: .anarlogHumans, using: humansEmptyRig.notifier)
        await assertSingleAdd(
            humansEmptyRig.presenter,
            identifier: AnarlogHealthNotifier.quietIdentifier,
            title: Self.quietTitle,
            body: Self.quietBody)

        let humansStaleRig = makeRig()
        await record(.clean(newestSessionCreatedAt: stale), from: .anarlogHumans, using: humansStaleRig.notifier)
        await assertSingleAdd(
            humansStaleRig.presenter,
            identifier: AnarlogHealthNotifier.quietIdentifier,
            title: Self.quietTitle,
            body: Self.quietBody)
    }

    // MAC-049 and MAC-053: an empty withheld listing raises both notifications.
    func testEmptyListingOverDeletionCapRaisesBothConditions() async {
        let rig = makeRig()
        await record(.deletionsWithheld(withheld: 9, newestSessionCreatedAt: nil), using: rig.notifier)
        let calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[8], AnarlogHealthNotifier.quietIdentifier])
        XCTAssertEqual(calls.map(\.title), [Self.brokenTitle, Self.quietTitle])
        XCTAssertEqual(calls.map(\.body), [
            "9 Anarlog session deletions were withheld because they exceed the deletion cap. To apply them, raise it with: crm-mac configure anarlog --deletion-cap",
            Self.quietBody,
        ])
        XCTAssertTrue(calls.allSatisfy { $0.userInfo.isEmpty && $0.sound })
    }

    // MAC-053: failures leave quiet unchanged; an undecided quiet state is swept on first report.
    func testFailedOutcomeLeavesQuietAloneAndInitialRuntimeFailureSweepsIt() async {
        let raisedRig = makeRig()
        await record(.clean(newestSessionCreatedAt: nil), using: raisedRig.notifier)
        await record(.failed(.binaryNotFound), using: raisedRig.notifier)
        let raisedRemovals = await removalCalls(raisedRig.presenter)
        XCTAssertFalse(raisedRemovals.flatMap { $0 }.contains(AnarlogHealthNotifier.quietIdentifier))
        await assertRemovalCallsEqual(raisedRig.presenter)

        let undecidedRig = makeRig()
        await record(.failed(.timeout), using: undecidedRig.notifier)
        let removals = await removalCalls(undecidedRig.presenter)
        XCTAssertEqual(removals, [Self.brokenIdentifiers])
        let pending = await undecidedRig.presenter.recordedRemovePending()
        XCTAssertEqual(removals, pending)
        let calls = await addCalls(undecidedRig.presenter)
        XCTAssertTrue(calls.isEmpty)
    }

    // S5: unknown presence is swept once, then stable absent state causes no further work.
    func testFirstReportSweepsUnknownIdentifiersOnce() async {
        let rig = makeRig()
        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        let addsAfterFirst = await addCalls(rig.presenter)
        let deliveredAfterFirst = await removalCalls(rig.presenter)
        let pendingAfterFirst = await rig.presenter.recordedRemovePending()
        XCTAssertTrue(addsAfterFirst.isEmpty)
        XCTAssertEqual(deliveredAfterFirst, [Self.brokenIdentifiers + [AnarlogHealthNotifier.quietIdentifier]])
        XCTAssertEqual(deliveredAfterFirst, pendingAfterFirst)
        let authorizationAfterFirst = await rig.presenter.recordedRequestAuthorizationCount()

        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        let addsAfterSecond = await addCalls(rig.presenter)
        let deliveredAfterSecond = await removalCalls(rig.presenter)
        let pendingAfterSecond = await rig.presenter.recordedRemovePending()
        let authorizationAfterSecond = await rig.presenter.recordedRequestAuthorizationCount()
        XCTAssertEqual(addsAfterSecond.count, addsAfterFirst.count)
        XCTAssertEqual(deliveredAfterSecond.count, deliveredAfterFirst.count)
        XCTAssertEqual(pendingAfterSecond.count, pendingAfterFirst.count)
        XCTAssertEqual(authorizationAfterSecond, authorizationAfterFirst)
        XCTAssertEqual(deliveredAfterSecond, pendingAfterSecond)
    }

    // MAC-042: denied authorization and failed add retry on a later report.
    func testDeniedAndFailedDeliveryRetry() async {
        let deniedRig = makeRig(authorizationResult: false)
        await record(.failed(.binaryNotFound), using: deniedRig.notifier)
        var calls = await addCalls(deniedRig.presenter)
        XCTAssertTrue(calls.isEmpty)
        await deniedRig.presenter.setAuthorizationResult(true)
        await record(.failed(.binaryNotFound), using: deniedRig.notifier)
        calls = await addCalls(deniedRig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[0]])
        await record(.failed(.binaryNotFound), using: deniedRig.notifier)
        calls = await addCalls(deniedRig.presenter)
        XCTAssertEqual(calls.count, 1)

        let failedRig = makeRig(addError: NSError(domain: "synthetic", code: 1))
        await record(.failed(.binaryNotFound), using: failedRig.notifier)
        calls = await addCalls(failedRig.presenter)
        XCTAssertEqual(calls.count, 1)
        await failedRig.presenter.setAddError(nil)
        await record(.failed(.binaryNotFound), using: failedRig.notifier)
        await record(.failed(.binaryNotFound), using: failedRig.notifier)
        calls = await addCalls(failedRig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[0], Self.brokenIdentifiers[0]])
    }

    // S5: reports arriving during an add are folded into one serialized reconcile loop.
    func testReconcilePassesAreSerialized() async throws {
        let rig = makeRig()
        await rig.presenter.armAddGate()
        let firstReport = Task {
            await rig.notifier.record(source: .anarlogSessions, outcome: .failed(.binaryNotFound))
        }

        var reads = 0
        while await rig.presenter.addsCurrentlyAwaitingGate() == 0 && reads < 200 {
            try await Task.sleep(nanoseconds: 10_000_000)
            reads += 1
        }
        let waiting = await rig.presenter.addsCurrentlyAwaitingGate()
        XCTAssertEqual(waiting, 1)

        let addCountBefore = await addCalls(rig.presenter).count
        let deliveredCountBefore = await removalCalls(rig.presenter).count
        let pendingCountBefore = await rig.presenter.recordedRemovePending().count
        await record(.failed(.binaryNotFound), using: rig.notifier)
        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        let addCountDuringGate = await addCalls(rig.presenter).count
        let deliveredCountDuringGate = await removalCalls(rig.presenter).count
        let pendingCountDuringGate = await rig.presenter.recordedRemovePending().count
        XCTAssertEqual(addCountDuringGate, addCountBefore)
        XCTAssertEqual(deliveredCountDuringGate, deliveredCountBefore)
        XCTAssertEqual(pendingCountDuringGate, pendingCountBefore)

        await rig.presenter.releaseAddGate()
        await firstReport.value
        let calls = await addCalls(rig.presenter)
        XCTAssertEqual(calls.map(\.identifier), [Self.brokenIdentifiers[0]])
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[0], AnarlogHealthNotifier.quietIdentifier])
        await assertRemovalCallsEqual(rig.presenter)
    }

    // I14: notifier operations only use anarlog_source identifiers.
    func testNotifierNeverRemovesOrAddsOrphanOrConflictIdentifiers() async {
        let rig = makeRig()
        await record(.failed(.binaryNotFound), using: rig.notifier)
        await record(.failed(.timeout), from: .anarlogHumans, using: rig.notifier)
        await record(.failed(.timeout), from: .anarlogHumans, using: rig.notifier)
        await record(.deletionsWithheld(withheld: 4, newestSessionCreatedAt: nil), using: rig.notifier)
        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        await record(.clean(newestSessionCreatedAt: recent), from: .anarlogHumans, using: rig.notifier)
        await record(.failed(.missingField(command: "meetings list", field: "data")), using: rig.notifier)
        await record(.clean(newestSessionCreatedAt: stale), using: rig.notifier)

        let adds = await addCalls(rig.presenter)
        let removals = await removalCalls(rig.presenter)
        XCTAssertFalse(removals.isEmpty)
        XCTAssertTrue(adds.allSatisfy { $0.identifier.hasPrefix("anarlog_source:") })
        XCTAssertTrue(removals.flatMap { $0 }.allSatisfy { $0.hasPrefix("anarlog_source:") })
        await assertRemovalCallsEqual(rig.presenter)
    }

    // I14: the existing orphan sweep leaves the new health identifiers alone.
    func testOrphanSweepDoesNotRemoveHealthIdentifiers() async throws {
        let presenter = FakeUserNotificationPresenter()
        let brokenID = "anarlog_source:broken:timeout"
        let quietID = AnarlogHealthNotifier.quietIdentifier
        let legacyOrphanID = "orphan:deadbeef-1111-2222-3333-444455556661"
        let identifiers = [brokenID, quietID, legacyOrphanID]
        await presenter.seedDeliveredIdentifiers(identifiers)
        await presenter.seedPendingIdentifiers(identifiers)

        let temporaryDirectory = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-anarlog-health-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: temporaryDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: temporaryDirectory) }
        let store = StateStore(fileURL: temporaryDirectory.appendingPathComponent("state.json"))
        try store.save(DaemonState())
        let mutator = StateMutator(store: store)
        let center = OrphanNotificationCenter(
            presenter: presenter,
            opener: FakeWorkspaceOpener(),
            mutator: mutator,
            metadataLookup: FakeSessionMetadataLookup(),
            piURL: URL(string: "https://pi.example")!,
            needsAttentionFetcher: { [] },
            logger: NoopLogger(),
            clock: { Self.now })

        await center.cleanupLegacyOSNotifications()

        let delivered = await presenter.recordedRemoveDelivered()
        let pending = await presenter.recordedRemovePending()
        XCTAssertTrue(delivered.flatMap { $0 }.contains(legacyOrphanID))
        XCTAssertTrue(pending.flatMap { $0 }.contains(legacyOrphanID))
        XCTAssertFalse(delivered.flatMap { $0 }.contains(brokenID))
        XCTAssertFalse(delivered.flatMap { $0 }.contains(quietID))
        XCTAssertFalse(pending.flatMap { $0 }.contains(brokenID))
        XCTAssertFalse(pending.flatMap { $0 }.contains(quietID))
        XCTAssertEqual(delivered, pending)
    }

    // MAC-052: denied or failed retries use the latest condition detail.
    func testRetryUsesLatestDetail() async {
        let deniedRig = makeRig(authorizationResult: false)
        await record(.deletionsWithheld(withheld: 3, newestSessionCreatedAt: recent), using: deniedRig.notifier)
        let noAdds = await addCalls(deniedRig.presenter)
        XCTAssertTrue(noAdds.isEmpty)
        await deniedRig.presenter.setAuthorizationResult(true)
        await record(.deletionsWithheld(withheld: 7, newestSessionCreatedAt: recent), using: deniedRig.notifier)
        await assertSingleAdd(
            deniedRig.presenter,
            identifier: Self.brokenIdentifiers[8],
            title: Self.brokenTitle,
            body: "7 Anarlog session deletions were withheld because they exceed the deletion cap. To apply them, raise it with: crm-mac configure anarlog --deletion-cap")

        let failedRig = makeRig(addError: NSError(domain: "synthetic", code: 1))
        await record(.failed(.unsupportedSchemaVersion("2")), using: failedRig.notifier)
        await failedRig.presenter.setAddError(nil)
        await record(.failed(.unsupportedSchemaVersion("3")), using: failedRig.notifier)
        let calls = await addCalls(failedRig.presenter)
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(calls[0].identifier, Self.brokenIdentifiers[1])
        XCTAssertEqual(calls[1].identifier, Self.brokenIdentifiers[1])
        XCTAssertEqual(calls[1].body, "The anarlog CLI reports contract version \"3\"; crm-mac supports \"1\".")
    }

    // MAC-053: a recent withheld-deletions listing clears quiet while raising its own condition.
    func testRecentWithheldListingClearsQuiet() async {
        let rig = makeRig()
        await record(.clean(newestSessionCreatedAt: nil), using: rig.notifier)
        await record(.deletionsWithheld(withheld: 5, newestSessionCreatedAt: recent), using: rig.notifier)
        let adds = await addCalls(rig.presenter)
        XCTAssertEqual(adds.map(\.identifier), [AnarlogHealthNotifier.quietIdentifier, Self.brokenIdentifiers[8]])
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [AnarlogHealthNotifier.quietIdentifier])
        XCTAssertEqual(
            removals.filter { $0.contains(Self.brokenIdentifiers[8]) }.count,
            1,
            "the initial unknown-presence sweep removes the previously absent broken identifier")
        await assertRemovalCallsEqual(rig.presenter)
    }

    // S5: withheld deletions replace the held condition and reset a runtime streak.
    func testWithheldOutcomeReplacesConditionAndResetsRuntimeStreak() async {
        let permanentRig = makeRig()
        await record(.failed(.binaryNotFound), using: permanentRig.notifier)
        await record(.deletionsWithheld(withheld: 2, newestSessionCreatedAt: recent), using: permanentRig.notifier)
        let permanentAdds = await addCalls(permanentRig.presenter)
        XCTAssertEqual(permanentAdds.map(\.identifier), [Self.brokenIdentifiers[0], Self.brokenIdentifiers[8]])
        let permanentRemovals = await removalCalls(permanentRig.presenter)
        XCTAssertEqual(permanentRemovals.last, [Self.brokenIdentifiers[0], AnarlogHealthNotifier.quietIdentifier])
        await assertRemovalCallsEqual(permanentRig.presenter)

        let runtimeRig = makeRig()
        await record(.failed(.timeout), using: runtimeRig.notifier)
        await record(.deletionsWithheld(withheld: 2, newestSessionCreatedAt: recent), using: runtimeRig.notifier)
        await record(.failed(.timeout), using: runtimeRig.notifier)
        let runtimeAdds = await addCalls(runtimeRig.presenter)
        XCTAssertEqual(runtimeAdds.map(\.identifier), [Self.brokenIdentifiers[8]])
        let runtimeRemovals = await removalCalls(runtimeRig.presenter)
        XCTAssertEqual(runtimeRemovals.last, [Self.brokenIdentifiers[8]])
        await assertRemovalCallsEqual(runtimeRig.presenter)
    }

    func testFailedAddLogsThrownError() async {
        let error = NSError(domain: "synthetic", code: 7)
        let presenter = FakeUserNotificationPresenter(addError: error)
        let logger = RecordingLogger()
        let notifier = AnarlogHealthNotifier(
            presenter: presenter,
            logger: logger,
            clock: { Self.now })

        await record(.failed(.binaryNotFound), using: notifier)

        let entries = logger.entries
        XCTAssertEqual(entries.count, 1)
        guard let entry = entries.first else { return }
        if case .warning = entry.0 {
        } else {
            XCTFail("expected a warning log")
        }
        XCTAssertEqual(
            entry.1,
            "anarlog-health: presenter.add threw; notification remains absent for retry")
        XCTAssertEqual(entry.2, [
            "identifier": .public("anarlog_source:broken:binary_not_found"),
            "error": .private(String(describing: error)),
        ])
    }

    func testRetryUsesDetailOfASourceStillHoldingTheCondition() async {
        let presenter = FakeUserNotificationPresenter(authorizationResult: false)
        let notifier = AnarlogHealthNotifier(
            presenter: presenter,
            logger: NoopLogger(),
            clock: { Self.now })

        await record(.failed(.unsupportedSchemaVersion("2")), using: notifier)
        await record(.failed(.unsupportedSchemaVersion("3")), from: .anarlogHumans, using: notifier)
        await presenter.setAuthorizationResult(true)
        await record(.clean(newestSessionCreatedAt: recent), from: .anarlogHumans, using: notifier)

        await assertSingleAdd(
            presenter,
            identifier: Self.brokenIdentifiers[1],
            title: Self.brokenTitle,
            body: "The anarlog CLI reports contract version \"2\"; crm-mac supports \"1\".")
    }

    func testConditionClearedDuringAuthorizationIsNotAdded() async throws {
        let rig = makeRig()
        await rig.presenter.armAuthorizationGate()
        let firstReport = Task {
            await rig.notifier.record(source: .anarlogSessions, outcome: .failed(.binaryNotFound))
        }

        var reads = 0
        while await rig.presenter.authorizationsCurrentlyAwaitingGate() == 0 && reads < 200 {
            try await Task.sleep(nanoseconds: 10_000_000)
            reads += 1
        }
        let waiting = await rig.presenter.authorizationsCurrentlyAwaitingGate()
        XCTAssertEqual(waiting, 1)

        await record(.clean(newestSessionCreatedAt: recent), using: rig.notifier)
        await rig.presenter.releaseAuthorizationGate()
        await firstReport.value

        let calls = await addCalls(rig.presenter)
        XCTAssertTrue(calls.isEmpty)
        let authorizationCount = await rig.presenter.recordedRequestAuthorizationCount()
        XCTAssertEqual(authorizationCount, 1)
        let removals = await removalCalls(rig.presenter)
        XCTAssertEqual(removals.last, [Self.brokenIdentifiers[0], AnarlogHealthNotifier.quietIdentifier])
        await assertRemovalCallsEqual(rig.presenter)
    }

    func testHealthNotificationTableCoversEveryIdentifier() {
        XCTAssertEqual(
            AnarlogHealthNotification.all.map(\.identifier),
            Self.brokenIdentifiers + [AnarlogHealthNotifier.quietIdentifier])
        XCTAssertEqual(Set(AnarlogHealthNotification.all).count, 10)
    }

    func testRetryUsesLatestDetailAcrossSources() async {
        let presenter = FakeUserNotificationPresenter(addError: NSError(domain: "synthetic", code: 1))
        let notifier = AnarlogHealthNotifier(
            presenter: presenter,
            logger: NoopLogger(),
            clock: { Self.now })

        await record(.failed(.unsupportedSchemaVersion("2")), using: notifier)
        await presenter.setAddError(nil)
        await record(.failed(.unsupportedSchemaVersion("3")), from: .anarlogHumans, using: notifier)

        let calls = await addCalls(presenter)
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(calls.map(\.identifier), [
            Self.brokenIdentifiers[1],
            Self.brokenIdentifiers[1],
        ])
        XCTAssertEqual(
            calls.last?.body,
            "The anarlog CLI reports contract version \"3\"; crm-mac supports \"1\".")
    }
}
