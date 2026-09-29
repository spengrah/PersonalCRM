import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogEligibilityTests: XCTestCase {
    func testFloorBoundary() {
        let floor = AnarlogEligibility.backfillFloor
        let now = floor.addingTimeInterval(AnarlogEligibility.settleInterval)

        XCTAssertTrue(AnarlogEligibility.isEligible(createdAt: floor, now: now))
        XCTAssertFalse(AnarlogEligibility.isEligible(
            createdAt: floor.addingTimeInterval(-0.001), now: now))
    }

    func testSettleBoundaryAndFutureSession() {
        let createdAt = AnarlogEligibility.backfillFloor.addingTimeInterval(86_400)

        XCTAssertTrue(AnarlogEligibility.isEligible(
            createdAt: createdAt,
            now: createdAt.addingTimeInterval(3 * 60 * 60)))
        XCTAssertFalse(AnarlogEligibility.isEligible(
            createdAt: createdAt,
            now: createdAt.addingTimeInterval(3 * 60 * 60 - 1)))
        XCTAssertFalse(AnarlogEligibility.isEligible(
            createdAt: createdAt.addingTimeInterval(1), now: createdAt))
    }

    func testParticipantFilterExcludesOperatorAndSelfAndKeepsOrder() {
        let first = AnarlogParticipant(personID: "person-a", displayName: "A", email: nil, jobTitle: nil)
        let operatorParticipant = AnarlogParticipant(
            personID: "operator-id", displayName: "Operator", email: nil, jobTitle: nil)
        let selfParticipant = AnarlogParticipant(
            personID: CRMMacAnarlogSource.selfHumanUUID, displayName: "Self", email: nil, jobTitle: nil)
        let last = AnarlogParticipant(personID: "person-b", displayName: "B", email: nil, jobTitle: nil)
        let record = AnarlogSessionRecord(
            id: "session-a",
            title: nil,
            createdAt: AnarlogEligibility.backfillFloor,
            memo: nil,
            summaries: [],
            participants: [first, operatorParticipant, selfParticipant, last])

        XCTAssertEqual(
            record.participants(excludingOperator: "OPERATOR-ID"),
            [first, last])
    }
}
