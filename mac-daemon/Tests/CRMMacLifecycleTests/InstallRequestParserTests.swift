import XCTest
@testable import CRMMacLifecycle

final class InstallRequestParserTests: XCTestCase {
    private func input(
        piURL: String = "https://pi.example.test",
        pair: String = "tk",
        hostname: String = "mac-1",
        upgrade: Bool = false,
        registerOnly: Bool = false
    ) -> InstallRequestParserInput {
        InstallRequestParserInput(
            piURL: piURL,
            pair: pair,
            hostname: hostname,
            upgrade: upgrade,
            registerOnly: registerOnly)
    }

    func testFreshHappyPath() throws {
        let r = try InstallRequestParser.parse(input())
        XCTAssertEqual(r.piURL.absoluteString, "https://pi.example.test")
        XCTAssertEqual(r.pairingToken, "tk")
        XCTAssertEqual(r.hostname, "mac-1")
        XCTAssertFalse(r.upgrade)
    }

    func testFreshRejectsEmptyPiURL() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(piURL: ""))) { e in
            XCTAssertEqual(e as? InstallRequestParseError, .piURLRequired)
        }
    }

    func testFreshRejectsEmptyPair() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(pair: ""))) { e in
            XCTAssertEqual(e as? InstallRequestParseError, .pairTokenRequired)
        }
    }

    func testFreshRejectsEmptyHostname() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(hostname: ""))) { e in
            XCTAssertEqual(e as? InstallRequestParseError, .hostnameRequired)
        }
    }

    func testFreshRejectsFileScheme() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(piURL: "file:///tmp"))) { e in
            guard case InstallRequestParseError.invalidPiURL = e else {
                XCTFail("expected invalidPiURL, got \(e)")
                return
            }
        }
    }

    func testFreshRejectsRelativePath() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(piURL: "/relative/path"))) { e in
            guard case InstallRequestParseError.invalidPiURL = e else {
                XCTFail("expected invalidPiURL, got \(e)")
                return
            }
        }
    }

    func testFreshAcceptsHTTPSWithPort() throws {
        _ = try InstallRequestParser.parse(input(piURL: "https://pi.example.test:8443"))
    }

    func testUpgradeIgnoresEmptyRequiredFields() throws {
        // --upgrade does not require pair / hostname / pi-url.
        let r = try InstallRequestParser.parse(input(
            piURL: "", pair: "", hostname: "", upgrade: true))
        XCTAssertTrue(r.upgrade)
        XCTAssertEqual(r.pairingToken, "")
        XCTAssertEqual(r.hostname, "")
    }

    func testUpgradeTolerantOfInvalidPiURL() throws {
        // --upgrade ignores a supplied pi-url; tolerate any value
        // including file:// schemes.
        let r = try InstallRequestParser.parse(input(
            piURL: "file:///tmp", pair: "", hostname: "", upgrade: true))
        XCTAssertTrue(r.upgrade)
    }

    func testUpgradeTolerantOfMalformedPiURL() throws {
        // Even a syntactically-malformed URL string passes — upgrade
        // doesn't consume it.
        let r = try InstallRequestParser.parse(input(
            piURL: " ", pair: "", hostname: "", upgrade: true))
        XCTAssertTrue(r.upgrade)
    }

    func testRegisterOnlyTolerantOfInvalidPiURL() throws {
        let r = try InstallRequestParser.parse(input(
            piURL: "file:///tmp", pair: "", hostname: "", registerOnly: true))
        XCTAssertTrue(r.registerOnly)
    }

    func testRegisterOnlyTolerantOfMalformedPiURL() throws {
        let r = try InstallRequestParser.parse(input(
            piURL: " ", pair: "", hostname: "", registerOnly: true))
        XCTAssertTrue(r.registerOnly)
    }

    func testMutuallyExclusiveModes() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(upgrade: true, registerOnly: true))) { e in
            XCTAssertEqual(e as? InstallRequestParseError, .mutuallyExclusiveModes)
        }
    }

    func testMalformedURL() {
        XCTAssertThrowsError(try InstallRequestParser.parse(input(piURL: " "))) { e in
            // URL(string: " ") returns nil — surfaces as malformedPiURL.
            guard case InstallRequestParseError.malformedPiURL = e else {
                XCTFail("expected malformedPiURL, got \(e)")
                return
            }
        }
    }
}
