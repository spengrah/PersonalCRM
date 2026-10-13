// The rescan-messages entry point, observed over a messages-database fixture
// with the Pi faked at PiClient's transport boundary.
import XCTest
import Foundation
import CRMMacPiClient

final class RescanMessagesTests: XCTestCase {
    func testRichTextOnlySourceBodyIsSubmittedAndCountedFilled() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: nil, attributedBody: typedstream("hello from rich text")),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.submittedGuids, ["g1"])
        XCTAssertEqual(pi.body(of: "g1"), "hello from rich text")
        XCTAssertEqual(result.report["listed"], "1")
        XCTAssertEqual(result.report["filled"], "1")
    }

    func testPlainTextSourceBodyIsSubmitted() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: "plain words", attributedBody: nil),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.body(of: "g1"), "plain words")
        XCTAssertEqual(result.report["filled"], "1")
    }

    func testAttachmentOnlySourceRowIsClassifiedByNatureAndNotSubmitted() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: "\u{FFFC}", attributedBody: nil, hasAttachment: true),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(result.report["by_nature.attachment_only"], "1")
        XCTAssertEqual(result.report["listed"], "1")
    }

    func testReactionRowWithNoStoredBodyIsClassifiedByNature() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: nil, attributedBody: nil, associatedMessageType: 2001),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(result.report["by_nature.reaction"], "1")
    }

    func testSourceRowWithNoBodyNoAttachmentNoReactionIsClassifiedNoneStored() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: nil, attributedBody: nil),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(result.report["by_nature.none_stored"], "1")
    }

    func testGuidWithNoSourceRowIsClassifiedUnavailable() async throws {
        let fixture = try RescanFixture(source: [])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(result.report["unavailable"], "1")
    }

    func testUndecodableRichTextBodyIsClassifiedCaptureFailureAndItsGuidListed() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: nil, attributedBody: undecodableBlob),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(result.report["capture_failure"], "1")
        XCTAssertEqual(result.output.captureFailureGuids, ["g1"])
    }

    func testListingSpanningTwoPagesHasBothPagesRead() async throws {
        let guids = (1...5).map { "g\($0)" }
        let fixture = try RescanFixture(source: guids.map {
            SourceRow(guid: $0, text: "body \($0)", attributedBody: nil)
        })
        let pi = FakePi(rows: Dictionary(uniqueKeysWithValues: guids.map { ($0, nil as String?) }),
                        pageCap: 2)

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.listRequests, 3)
        XCTAssertEqual(Set(pi.submittedGuids), Set(guids))
        XCTAssertEqual(result.report["listed"], "5")
        XCTAssertEqual(result.report["filled"], "5")
    }

    func test501RecoverableRowsAreSubmittedInRequestsOfAtMost500() async throws {
        let guids = (0...500).map { String(format: "g%04d", $0) }
        let fixture = try RescanFixture(source: guids.map {
            SourceRow(guid: $0, text: "body \($0)", attributedBody: nil)
        })
        let pi = FakePi(rows: Dictionary(uniqueKeysWithValues: guids.map { ($0, nil as String?) }))

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertGreaterThan(pi.fillRequests.count, 1)
        XCTAssertTrue(pi.fillRequests.allSatisfy { $0.count <= 500 })
        XCTAssertEqual(pi.submittedGuids.count, 501)
        XCTAssertEqual(result.report["filled"], "501")
    }

    func testBodiesThatEscapeToMoreThanTheRequestCapAreSplitUnder8MiB() async throws {
        let guids = (1...4).map { "g\($0)" }
        let body = String(repeating: "\u{01}", count: 400_000)
        let fixture = try RescanFixture(source: guids.map {
            SourceRow(guid: $0, text: body, attributedBody: nil)
        })
        let pi = FakePi(rows: Dictionary(uniqueKeysWithValues: guids.map { ($0, nil as String?) }))

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertGreaterThan(pi.fillRequests.count, 1)
        for request in pi.fillRequests {
            struct Body: Encodable { let items: [FillBodyItem] }
            let encoded = try JSONEncoder().encode(Body(items: request))
            XCTAssertLessThan(encoded.count, 8 << 20)
        }
        XCTAssertEqual(result.report["filled"], "4")
    }

    func testASingleBodyThatCannotFitInOneRequestEndsTheRunNonZeroWithNoReport() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: String(repeating: "\u{01}", count: 1_500_000), attributedBody: nil),
        ])
        let pi = FakePi(rows: ["g1": nil])

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertTrue(result.output.stdoutText.isEmpty, "no completed classification reported")
    }

    func testSubmittedRowThePiAnswersUnknownIsCountedUnknown() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: "still at the source", attributedBody: nil),
        ])
        let pi = FakePi(rows: ["g1": nil])
        pi.afterListing = { bodies in bodies["g1"] = nil }

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.submittedGuids, ["g1"])
        XCTAssertEqual(result.report["unknown"], "1")
        XCTAssertEqual(result.report["filled"], "0")
        XCTAssertEqual(result.report["unavailable"], "0")
    }

    func testSubmittedRowThePiAnswersUnchangedIsCountedUnchangedAndNotClassified() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: "source body", attributedBody: nil),
        ])
        let pi = FakePi(rows: ["g1": nil])
        pi.afterListing = { bodies in bodies["g1"] = "gained after the listing" }

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.body(of: "g1"), "gained after the listing")
        XCTAssertEqual(result.report["unchanged"], "1")
        XCTAssertEqual(result.report["filled"], "0")
        XCTAssertEqual(result.report["unavailable"], "0")
        XCTAssertEqual(result.report["capture_failure"], "0")
        for nature in ["attachment_only", "reaction", "none_stored"] {
            XCTAssertEqual(result.report["by_nature.\(nature)"], "0")
        }
    }

    /// One run over every outcome and every classification.
    private func mixedFixture() throws -> (RescanFixture, FakePi) {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "filled-rich", text: nil, attributedBody: typedstream("secret rich words")),
            SourceRow(guid: "filled-plain", text: "secret plain words", attributedBody: nil),
            SourceRow(guid: "unchanged", text: "secret source words", attributedBody: nil),
            SourceRow(guid: "unknown", text: "secret unknown words", attributedBody: nil),
            SourceRow(guid: "att", text: nil, attributedBody: nil, hasAttachment: true),
            SourceRow(guid: "react", text: nil, attributedBody: nil, associatedMessageType: 3002),
            SourceRow(guid: "none", text: nil, attributedBody: nil),
            SourceRow(guid: "undecodable", text: nil, attributedBody: undecodableBlob),
        ])
        let pi = FakePi(
            rows: ["filled-rich": nil, "filled-plain": nil, "unchanged": nil, "att": nil,
                   "react": nil, "none": nil, "undecodable": nil, "no-source-row": nil,
                   "unknown": nil])
        pi.afterListing = { bodies in
            bodies["unchanged"] = "already there"
            bodies["unknown"] = nil
        }
        return (fixture, pi)
    }

    func testRunMixingEveryOutcomeAndClassificationReportsCountsSummingToTheListedRows() async throws {
        let (fixture, pi) = try mixedFixture()
        let before = try fileTree(fixture.home)

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        let r = result.report
        let counts = ["filled", "unchanged", "unknown", "by_nature.attachment_only", "by_nature.reaction",
                      "by_nature.none_stored", "unavailable", "capture_failure"].map { Int(r[$0]!)! }
        XCTAssertEqual(counts, [2, 1, 1, 1, 1, 1, 1, 1])
        XCTAssertEqual(counts.reduce(0, +), Int(r["listed"]!))
        XCTAssertEqual(r["listed"], "9")
        XCTAssertEqual(result.output.captureFailureGuids, ["undecodable"])
        // Content-free output, and no file written.
        for secret in ["secret", "rich words", "plain words", "already there"] {
            XCTAssertFalse(result.output.stdoutText.contains(secret))
            XCTAssertFalse(result.output.stderrText.contains(secret))
        }
        XCTAssertEqual(try fileTree(fixture.home), before)
    }

    func testSecondRunAgainstThePiStateTheFirstLeftSubmitsNothingAndReportsTheSameClassification() async throws {
        let (fixture, pi) = try mixedFixture()
        let first = await fixture.run(pi: pi)
        XCTAssertEqual(pi.fillRequests.count, 1)

        let second = await fixture.run(pi: pi)

        XCTAssertEqual(second.exitCode, 0)
        XCTAssertEqual(pi.fillRequests.count, 1, "second run submits nothing")
        for key in ["by_nature.attachment_only", "by_nature.reaction", "by_nature.none_stored",
                    "unavailable", "capture_failure"] {
            XCTAssertEqual(second.report[key], first.report[key], key)
        }
        XCTAssertEqual(second.output.captureFailureGuids, first.output.captureFailureGuids)
        XCTAssertEqual(second.report["listed"], "5")
        for key in ["filled", "unchanged", "unknown"] {
            XCTAssertEqual(second.report[key], "0", key)
        }
    }

    func testPiRequestRejectedEndsTheRunNonZeroWithNoFurtherSubmissionAndNoReport() async throws {
        let guids = (1...4).map { "g\($0)" }
        let fixture = try RescanFixture(source: guids.map {
            SourceRow(guid: $0, text: "body \($0)", attributedBody: nil)
        })
        let pi = FakePi(rows: Dictionary(uniqueKeysWithValues: guids.map { ($0, nil as String?) }),
                        pageCap: 2, failure: .fill(1))

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertEqual(pi.fillRequests.count, 1)
        XCTAssertEqual(pi.listRequests, 1)
        XCTAssertTrue(result.output.stdoutText.isEmpty, "no completed classification reported")
    }

    func testPiUnreachableEndsTheRunNonZeroWithNoSubmission() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "g1", text: "body", attributedBody: nil),
        ])
        let pi = FakePi(rows: ["g1": nil], failure: .unreachable)

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertTrue(result.output.stdoutText.isEmpty)
    }

    private func fileTree(_ root: URL) throws -> [String] {
        let enumerator = FileManager.default.enumerator(atPath: root.path)
        return (enumerator?.allObjects as? [String] ?? []).sorted()
    }
}
