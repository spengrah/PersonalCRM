// SourceIDBuilderTests cover the positive-identity contract:
// given identifier X and hash Y, upsertSourceID returns "X@Y" and
// deleteSourceID returns "X@deleted@Y" (or "X@deleted@unknown"
// when no prior hash is known). The Pi's acceptance pattern is
// `^[^@]+@...`, so identifiers with hyphens, underscores, and
// CNContact's typical alphanumeric-mixed shape all flow through
// unchanged.
import XCTest
@testable import CRMMacIcloudContactsSource

final class SourceIDBuilderTests: XCTestCase {

    func testUpsertSourceIDConcatenation() {
        let got = SourceIDBuilder.upsertSourceID(
            entityID: "contact-A", contentHash: "abcdef")
        XCTAssertEqual(got, "contact-A@abcdef")
    }

    func testUpsertSourceIDPreservesUnderscoreAndHyphen() {
        let got = SourceIDBuilder.upsertSourceID(
            entityID: "AB12_3-4", contentHash: "0123456789abcdef")
        XCTAssertEqual(got, "AB12_3-4@0123456789abcdef")
    }

    func testDeleteSourceIDWithKnownPrior() {
        let got = SourceIDBuilder.deleteSourceID(
            entityID: "contact-A", priorContentHash: "abcdef")
        XCTAssertEqual(got, "contact-A@deleted@abcdef")
    }

    func testDeleteSourceIDWithNilPriorUsesUnknownSentinel() {
        let got = SourceIDBuilder.deleteSourceID(
            entityID: "contact-A", priorContentHash: nil)
        XCTAssertEqual(got, "contact-A@deleted@unknown")
    }

    func testDeleteSourceIDPreservesEntityShape() {
        let got = SourceIDBuilder.deleteSourceID(
            entityID: "AB12_3-4", priorContentHash: "h")
        XCTAssertEqual(got, "AB12_3-4@deleted@h")
    }
}
