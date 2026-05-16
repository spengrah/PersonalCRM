// NormalizationParityTests round-trips the SHARED fixture at
// backend/internal/matching/testdata/normalization_parity.json through
// the Swift normalizers (CRMMacCore.NormalizationParity).
//
// The same JSON file is consumed by backend/internal/matching/parity_test.go
// on the Go side, so any drift between the two implementations fails
// loudly on whichever side runs first in CI.
//
// Path resolution uses #filePath (NOT #file) because SE-0274 changed
// #file in Swift 6 to a concise module/file string. We need the absolute
// path of THIS source file at compile time so we can walk up to the repo
// root and find the fixture.
import XCTest
@testable import CRMMacCore

final class NormalizationParityTests: XCTestCase {
    private struct ParityEntry: Decodable {
        let raw: String
        let type: String
        let expected: String
    }

    func testFixtureRoundTrip() throws {
        let fixtureURL = NormalizationParityTests.resolveFixtureURL()
        try assertFixtureExists(fixtureURL)
        let data = try Data(contentsOf: fixtureURL)
        let entries = try JSONDecoder().decode([ParityEntry].self, from: data)
        XCTAssertFalse(entries.isEmpty, "parity fixture must contain entries")

        for entry in entries {
            let got: String
            switch entry.type {
            case "email":
                got = NormalizationParity.normalizeEmail(entry.raw)
            case "phone":
                got = NormalizationParity.normalizePhoneE164(entry.raw)
            default:
                XCTFail("unknown parity entry type: \(entry.type)")
                continue
            }
            XCTAssertEqual(got, entry.expected,
                           "parity drift: type=\(entry.type) raw=\(entry.raw) " +
                           "got=\(got) expected=\(entry.expected)")
        }
    }

    // MARK: - helpers

    /// Walks four directories up from THIS source file:
    ///   .../mac-daemon/Tests/CRMMacCoreTests/NormalizationParityTests.swift
    ///     -> .../CRMMacCoreTests/
    ///     -> .../Tests/
    ///     -> .../mac-daemon/
    ///     -> .../<repo-root>/
    /// then appends backend/internal/matching/testdata/normalization_parity.json.
    private static func resolveFixtureURL() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("backend/internal/matching/testdata/normalization_parity.json")
    }

    private func assertFixtureExists(_ url: URL) throws {
        guard FileManager.default.fileExists(atPath: url.path) else {
            XCTFail("parity fixture missing at \(url.path) — did someone move the file?")
            throw NSError(domain: "ParityFixture", code: 1)
        }
    }
}
