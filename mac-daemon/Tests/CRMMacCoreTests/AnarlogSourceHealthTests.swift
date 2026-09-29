import XCTest
@testable import CRMMacCore

final class AnarlogSourceHealthTests: XCTestCase {
    func testFailurePermanenceClassification() {
        XCTAssertTrue(AnarlogCLIFailure.binaryNotFound.isPermanent)
        XCTAssertTrue(AnarlogCLIFailure.unsupportedSchemaVersion("2").isPermanent)
        XCTAssertTrue(AnarlogCLIFailure.missingField(command: "meetings get", field: "data").isPermanent)
        XCTAssertTrue(AnarlogCLIFailure.malformedOutput(command: "meetings list").isPermanent)
        XCTAssertTrue(AnarlogCLIFailure.operatorPersonIDUnset.isPermanent)
        XCTAssertFalse(AnarlogCLIFailure.databaseNotFound.isPermanent)
        XCTAssertFalse(AnarlogCLIFailure.nonZeroExit(code: 1, errorCode: "internal").isPermanent)
        XCTAssertFalse(AnarlogCLIFailure.timeout.isPermanent)
    }
}
