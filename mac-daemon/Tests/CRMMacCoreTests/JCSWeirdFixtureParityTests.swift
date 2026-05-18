// JCSWeirdFixtureParityTests anchor the Swift JCSCanonicalizer against
// the upstream `gowebpki/jcs@v1.0.1/testdata/{input,output}/weird.json`
// discriminator fixture. The pair is committed to
// `backend/internal/service/testdata/jcs_weird_{input,output}.json` so
// both this test AND the Go-side TestExternalContactHash_WeirdFixtureParity
// read the same bytes.
//
// The `weird` fixture exercises edge cases the daemon's payload
// contract doesn't itself produce — control escapes, supplementary-
// plane keys, `</script>` literal, `` control character — but
// that the canonicalizer must handle correctly to remain byte-identical
// with `gowebpki/jcs`.
import XCTest
@testable import CRMMacCore

final class JCSWeirdFixtureParityTests: XCTestCase {

    func testWeirdFixtureBytePassthrough() throws {
        let inputURL = Self.weirdInputURL()
        let outputURL = Self.weirdOutputURL()
        guard FileManager.default.fileExists(atPath: inputURL.path),
              FileManager.default.fileExists(atPath: outputURL.path) else {
            XCTFail("weird-fixture files missing; expected at " +
                    "backend/internal/service/testdata/jcs_weird_{input,output}.json")
            return
        }
        let input = try Data(contentsOf: inputURL)
        let expectedOutput = try Data(contentsOf: outputURL)

        let canonical = try JCSCanonicalizer.canonicalize(input)

        // Some shells / tools add a trailing newline on copy. Normalize
        // the comparison by trimming a single trailing 0x0a from the
        // expected payload before equality.
        let expectedTrimmed = trimTrailingNewline(expectedOutput)
        XCTAssertEqual(canonical, expectedTrimmed,
                       "Swift canonicalizer drift vs gowebpki/jcs weird.json fixture")
    }

    /// Explicit per-discriminator-case smoke check so a single failure
    /// surfaces with a meaningful site rather than a wall of bytes.
    func testWeirdFixtureKeyOrdering() throws {
        let inputURL = Self.weirdInputURL()
        let input = try Data(contentsOf: inputURL)
        let canonical = try JCSCanonicalizer.canonicalize(input)
        guard let s = String(data: canonical, encoding: .utf8) else {
            XCTFail("canonical not UTF-8")
            return
        }
        // The first key in sort order is "\n" (U+000A), then "\r"
        // (U+000D), then "1" (U+0031), then "</script>" (U+003C…),
        // then  (U+007F control), then "ö" (U+00F6), then "€"
        // (U+20AC), then "😂" (U+1F602; UTF-16 leading surrogate
        // 0xD83D sorts ABOVE BMP), then "דּ" (U+FB33).
        XCTAssertTrue(s.hasPrefix("{\"\\n\":\"Newline\""),
                      "first key should be \\n; got \(s.prefix(40))")
        XCTAssertTrue(s.contains("\"</script>\":\"Browser Challenge\""),
                      "</script> key must appear unescaped in output")
        XCTAssertTrue(s.contains("\"😂\":\"Smiley\""),
                      "emoji key must appear as literal UTF-8 in output")
    }

    // MARK: - helpers

    private func trimTrailingNewline(_ data: Data) -> Data {
        if data.last == 0x0A {
            return data.dropLast()
        }
        return data
    }

    private static func weirdInputURL() -> URL {
        repoRootURL().appendingPathComponent(
            "backend/internal/service/testdata/jcs_weird_input.json")
    }

    private static func weirdOutputURL() -> URL {
        repoRootURL().appendingPathComponent(
            "backend/internal/service/testdata/jcs_weird_output.json")
    }

    private static func repoRootURL() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
    }
}
