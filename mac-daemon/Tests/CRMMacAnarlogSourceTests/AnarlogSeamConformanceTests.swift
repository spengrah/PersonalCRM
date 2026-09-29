import XCTest
import CRMMacCore
@testable import CRMMacAnarlogSource

private struct SeamStubClient: AnarlogCLIClient {
    private let entry = AnarlogSessionListEntry(id: "session-a", createdAt: Date(timeIntervalSince1970: 1))
    private let record = AnarlogSessionRecord(
        id: "session-a",
        title: "Title A",
        createdAt: Date(timeIntervalSince1970: 1),
        memo: "Memo A",
        summaries: ["Summary A"],
        participants: [AnarlogParticipant(
            personID: "person-a", displayName: "Participant A", email: "a@example.test", jobTitle: "Role A")])

    func listSessions() async throws(AnarlogCLIFailure) -> [AnarlogSessionListEntry] {
        [entry]
    }

    func getSession(id: String) async throws(AnarlogCLIFailure) -> AnarlogSessionRecord? {
        id == record.id ? record : nil
    }
}

private func caseName(_ failure: AnarlogCLIFailure) -> String {
    switch failure {
    case .binaryNotFound:
        return "binaryNotFound"
    case .unsupportedSchemaVersion(let version):
        let _: String = version
        return "unsupportedSchemaVersion"
    case .missingField(command: let command, field: let field):
        let _: String = command
        let _: String = field
        return "missingField"
    case .malformedOutput(command: let command):
        let _: String = command
        return "malformedOutput"
    case .operatorPersonIDUnset:
        return "operatorPersonIDUnset"
    case .databaseNotFound:
        return "databaseNotFound"
    case .nonZeroExit(code: let code, errorCode: let errorCode):
        let _: Int32 = code
        let _: String? = errorCode
        return "nonZeroExit"
    case .timeout:
        return "timeout"
    }
}

private func caseName(_ outcome: AnarlogTickOutcome) -> String {
    switch outcome {
    case .clean(newestSessionCreatedAt: let newest):
        let _: Date? = newest
        return "clean"
    case .deletionsWithheld(withheld: let withheld, newestSessionCreatedAt: let newest):
        let _: Int = withheld
        let _: Date? = newest
        return "deletionsWithheld"
    case .failed(let failure):
        let _: AnarlogCLIFailure = failure
        return "failed"
    }
}

private func requireSendableEquatable<T: Sendable & Equatable>(_: T.Type) {}
private func requireError<T: Error>(_: T.Type) {}

final class AnarlogSeamConformanceTests: XCTestCase {
    func testClientProtocolSignaturesAndValues() async throws {
        let client: any AnarlogCLIClient = SeamStubClient()
        let entries = try await client.listSessions()
        XCTAssertEqual(entries, [AnarlogSessionListEntry(id: "session-a", createdAt: Date(timeIntervalSince1970: 1))])

        let record = try await client.getSession(id: "session-a")
        XCTAssertEqual(record?.id, "session-a")
        let missingRecord = try await client.getSession(id: "session-b")
        XCTAssertNil(missingRecord)
    }

    func testS3InitializersAndProperties() {
        let date = Date(timeIntervalSince1970: 42)
        let entry = AnarlogSessionListEntry(id: "session-b", createdAt: date)
        let entryID: String = entry.id
        let entryCreatedAt: Date = entry.createdAt
        XCTAssertEqual(entryID, "session-b")
        XCTAssertEqual(entryCreatedAt, date)

        let participant = AnarlogParticipant(
            personID: "person-b", displayName: "Participant B", email: "b@example.test", jobTitle: "Role B")
        let personID: String = participant.personID
        let displayName: String? = participant.displayName
        let email: String? = participant.email
        let jobTitle: String? = participant.jobTitle
        XCTAssertEqual(personID, "person-b")
        XCTAssertEqual(displayName, "Participant B")
        XCTAssertEqual(email, "b@example.test")
        XCTAssertEqual(jobTitle, "Role B")

        let record = AnarlogSessionRecord(
            id: "session-c",
            title: "Title C",
            createdAt: date,
            memo: "Memo C",
            summaries: ["Summary C"],
            participants: [participant])
        let recordID: String = record.id
        let title: String? = record.title
        let createdAt: Date = record.createdAt
        let memo: String? = record.memo
        let summaries: [String] = record.summaries
        let participants: [AnarlogParticipant] = record.participants
        XCTAssertEqual(recordID, "session-c")
        XCTAssertEqual(title, "Title C")
        XCTAssertEqual(createdAt, date)
        XCTAssertEqual(memo, "Memo C")
        XCTAssertEqual(summaries, ["Summary C"])
        XCTAssertEqual(participants, [participant])
    }

    func testFailureCasesAndAssociatedValues() {
        XCTAssertEqual(caseName(.binaryNotFound), "binaryNotFound")
        XCTAssertEqual(caseName(.unsupportedSchemaVersion("2")), "unsupportedSchemaVersion")
        XCTAssertEqual(caseName(.missingField(command: "meetings get", field: "data")), "missingField")
        XCTAssertEqual(caseName(.malformedOutput(command: "meetings list")), "malformedOutput")
        XCTAssertEqual(caseName(.operatorPersonIDUnset), "operatorPersonIDUnset")
        XCTAssertEqual(caseName(.databaseNotFound), "databaseNotFound")
        XCTAssertEqual(caseName(.nonZeroExit(code: 1, errorCode: "internal")), "nonZeroExit")
        XCTAssertEqual(caseName(.timeout), "timeout")
    }

    func testTickOutcomeCasesAndAssociatedValues() {
        let date = Date(timeIntervalSince1970: 50)
        XCTAssertEqual(caseName(.clean(newestSessionCreatedAt: date)), "clean")
        XCTAssertEqual(
            caseName(.deletionsWithheld(withheld: 2, newestSessionCreatedAt: date)),
            "deletionsWithheld")
        XCTAssertEqual(caseName(.failed(.timeout)), "failed")
    }

    func testSendableEquatableAndErrorConformances() {
        requireSendableEquatable(AnarlogSessionListEntry.self)
        requireSendableEquatable(AnarlogParticipant.self)
        requireSendableEquatable(AnarlogSessionRecord.self)
        requireSendableEquatable(AnarlogCLIFailure.self)
        requireSendableEquatable(AnarlogTickOutcome.self)
        requireError(AnarlogCLIFailure.self)
    }

    func testEligibilityTypedReferences() {
        let floor: Date = AnarlogEligibility.backfillFloor
        let settle: TimeInterval = AnarlogEligibility.settleInterval
        let eligible: Bool = AnarlogEligibility.isEligible(
            createdAt: floor,
            now: floor.addingTimeInterval(settle))
        XCTAssertTrue(eligible)
    }

    func testNoopSinkReturnsForEveryOutcomeAndHasNoState() async {
        let sink: any AnarlogHealthSink = NoopAnarlogHealthSink()
        let source = SourceID(rawValue: "anarlog_sessions")

        await sink.record(source: source, outcome: .clean(newestSessionCreatedAt: nil))
        await sink.record(source: source, outcome: .deletionsWithheld(withheld: 2, newestSessionCreatedAt: nil))
        await sink.record(source: source, outcome: .failed(.timeout))

        XCTAssertEqual(MemoryLayout<NoopAnarlogHealthSink>.size, 0)
    }
}
