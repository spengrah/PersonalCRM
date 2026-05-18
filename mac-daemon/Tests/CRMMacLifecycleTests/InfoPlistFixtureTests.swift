// InfoPlistFixtureTests validates the canonical Info.plist source file
// at mac-daemon/Sources/crm-mac/Info.plist. The file is the single
// source of truth for both the linker -sectcreate embed AND the bundle
// assembly (per plan D15 + D22). The test reads it directly via
// #filePath + relative descent rather than as a SwiftPM resource —
// the file is a BUILD-TIME input to the executable target, not a
// runtime resource of any test target.
import XCTest
@testable import CRMMacLifecycle

final class InfoPlistFixtureTests: XCTestCase {
    /// The 10 required keys per plan D2.
    private static let requiredKeys: [String] = [
        "CFBundleIdentifier",
        "CFBundleName",
        "CFBundleExecutable",
        "CFBundleVersion",
        "CFBundleShortVersionString",
        "CFBundlePackageType",
        "CFBundleInfoDictionaryVersion",
        "LSUIElement",
        "LSMinimumSystemVersion",
        "NSContactsUsageDescription",
    ]

    /// Resolve the canonical Info.plist by walking up from this test
    /// file. The directory layout is fixed by the SwiftPM package:
    ///   mac-daemon/Tests/CRMMacLifecycleTests/InfoPlistFixtureTests.swift
    ///   mac-daemon/Sources/crm-mac/Info.plist
    /// Both are package-root relative; we walk up 3 levels from the
    /// test file and descend into Sources/crm-mac/.
    private func loadInfoPlist() throws -> [String: Any] {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // CRMMacLifecycleTests/
            .deletingLastPathComponent()  // Tests/
            .deletingLastPathComponent()  // mac-daemon/
            .appendingPathComponent("Sources/crm-mac/Info.plist")
        let data = try Data(contentsOf: url)
        let any = try PropertyListSerialization.propertyList(
            from: data, options: [], format: nil)
        guard let dict = any as? [String: Any] else {
            throw NSError(
                domain: "InfoPlistFixtureTests",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "Info.plist did not parse to a dict"])
        }
        return dict
    }

    func testSourceFileParsesAsPropertyList() throws {
        let dict = try loadInfoPlist()
        XCTAssertFalse(dict.isEmpty, "Info.plist must parse to a non-empty dict")
    }

    func testRequiredKeysPresent() throws {
        let dict = try loadInfoPlist()
        for key in Self.requiredKeys {
            XCTAssertNotNil(dict[key], "Info.plist must contain key \(key)")
        }
    }

    func testCFBundleIdentifierMatchesDaemonLabel() throws {
        let dict = try loadInfoPlist()
        XCTAssertEqual(dict["CFBundleIdentifier"] as? String, Daemon.label)
    }

    func testLSUIElementIsTrue() throws {
        let dict = try loadInfoPlist()
        // plist <true/> decodes as Bool true (or NSNumber 1).
        if let b = dict["LSUIElement"] as? Bool {
            XCTAssertTrue(b, "LSUIElement must be true (agent app behavior)")
        } else if let n = dict["LSUIElement"] as? NSNumber {
            XCTAssertEqual(n.boolValue, true)
        } else {
            XCTFail("LSUIElement is not a Bool/NSNumber: \(String(describing: dict["LSUIElement"]))")
        }
    }

    func testNSContactsUsageDescriptionNonEmpty() throws {
        let dict = try loadInfoPlist()
        let value = dict["NSContactsUsageDescription"] as? String ?? ""
        XCTAssertFalse(value.isEmpty,
            "NSContactsUsageDescription must be a non-empty user-facing string")
    }

    func testLSMinimumSystemVersionAtLeast14() throws {
        let dict = try loadInfoPlist()
        // Plan targets macOS 14. A future bump to macOS 15 is fine;
        // a regression below 14 would break SMAppService API usage.
        let raw = dict["LSMinimumSystemVersion"] as? String ?? ""
        XCTAssertFalse(raw.isEmpty)
        let major = raw.split(separator: ".").first.map(String.init) ?? ""
        let majorInt = Int(major) ?? 0
        XCTAssertGreaterThanOrEqual(majorInt, 14,
            "LSMinimumSystemVersion must be 14.0 or greater (SMAppService requires macOS 13+; Package.swift targets 14)")
    }
}
