// Coverage for AnarlogTimestampParser. The four shapes are the ones
// observed in real Anarlog data (per the inline notes in the parser):
// `Z`+millis, `Z`+seconds, offset+micros, `Z`+micros.
import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogTimestampParserTests: XCTestCase {

    func testMillisecondsWithZ() throws {
        let raw = "2026-03-16T20:34:49.936Z"
        let date = try XCTUnwrap(AnarlogTimestampParser.parse(raw))
        // Spot-check via ISO8601 with fractional seconds — both paths
        // should agree to within < 1ms.
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        XCTAssertEqual(f.string(from: date), raw)
    }

    func testSecondsWithZ() throws {
        let raw = "2026-03-16T20:34:49Z"
        let date = try XCTUnwrap(AnarlogTimestampParser.parse(raw))
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        XCTAssertEqual(f.string(from: date), raw)
    }

    func testMicrosecondsWithOffset() throws {
        // Anarlog emits microsecond precision for `_meta.json` /
        // human-frontmatter `created_at`. We truncate to milliseconds
        // since we never compare timestamps at sub-millisecond
        // resolution.
        let raw = "2026-03-04T07:40:49.531658+00:00"
        let date = try XCTUnwrap(AnarlogTimestampParser.parse(raw))
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        XCTAssertEqual(f.string(from: date), "2026-03-04T07:40:49.531Z")
    }

    func testMicrosecondsWithZ() throws {
        let raw = "2026-03-04T07:40:49.531658Z"
        let date = try XCTUnwrap(AnarlogTimestampParser.parse(raw))
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        XCTAssertEqual(f.string(from: date), "2026-03-04T07:40:49.531Z")
    }

    func testNoFractionalWithOffset() throws {
        let raw = "2026-03-16T20:34:49+00:00"
        let date = try XCTUnwrap(AnarlogTimestampParser.parse(raw))
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        XCTAssertEqual(f.string(from: date), "2026-03-16T20:34:49Z")
    }

    func testNonZeroOffsetParses() throws {
        let raw = "2026-03-16T20:34:49.123456-05:00"
        let date = try XCTUnwrap(AnarlogTimestampParser.parse(raw))
        // 20:34 -05:00 == 01:34 next day UTC; fractional truncated to ms.
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        XCTAssertEqual(f.string(from: date), "2026-03-17T01:34:49.123Z")
    }

    func testEmptyReturnsNil() {
        XCTAssertNil(AnarlogTimestampParser.parse(""))
    }

    func testMalformedReturnsNil() {
        XCTAssertNil(AnarlogTimestampParser.parse("not-a-date"))
        XCTAssertNil(AnarlogTimestampParser.parse("2026/03/16"))
        XCTAssertNil(AnarlogTimestampParser.parse("16 Mar 2026"))
    }
}
