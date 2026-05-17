// ContainerPickerTests cover the install / configure picker's pure
// logic — defaults computation per D-JC9, render formatting, and the
// parse(input:containers:) accepts/rejects matrix. No I/O.
import XCTest
import CRMMacCore
@testable import CRMMacIcloudContactsSource

final class ContainerPickerTests: XCTestCase {

    private let iCloudContainer = ContainerInfo(
        identifier: "1B7C9", name: "iCloud", type: .cardDAV, defaultIncluded: true)
    private let onMyMacContainer = ContainerInfo(
        identifier: "D0E2A", name: "On My Mac", type: .local, defaultIncluded: true)
    private let googleContainer = ContainerInfo(
        identifier: "F2C7B", name: "Google", type: .cardDAV, defaultIncluded: false)
    private let exchangeContainer = ContainerInfo(
        identifier: "EX001", name: "Work Exchange", type: .exchange, defaultIncluded: false)
    private let unassignedContainer = ContainerInfo(
        identifier: "UN001", name: "Legacy", type: .unassigned, defaultIncluded: false)
    private let unknownContainer = ContainerInfo(
        identifier: "UK001", name: "Future", type: .unknown(rawValue: 99), defaultIncluded: false)

    // MARK: - defaults

    func testDefaultsIncludesLocalAndExactICloudCardDAV() {
        let defaults = ContainerPicker.defaults(for: [
            iCloudContainer, onMyMacContainer, googleContainer, exchangeContainer,
        ])
        XCTAssertEqual(Set(defaults), Set([iCloudContainer.identifier, onMyMacContainer.identifier]))
    }

    func testDefaultsExcludesGoogleAndExchangeAndUnassignedAndUnknown() {
        let defaults = ContainerPicker.defaults(for: [
            googleContainer, exchangeContainer, unassignedContainer, unknownContainer,
        ])
        XCTAssertTrue(defaults.isEmpty)
    }

    func testDefaultsExcludesCardDAVWithNonExactICloudName() {
        let fake = ContainerInfo(
            identifier: "X", name: "Fake iCloud Mirror", type: .cardDAV, defaultIncluded: false)
        let defaults = ContainerPicker.defaults(for: [fake])
        XCTAssertTrue(defaults.isEmpty,
                      "non-exact iCloud name must NOT default-include (locale-sensitive substring matching is disallowed)")
    }

    func testDefaultsCardDAVICloudNameMatchIsCaseInsensitive() {
        let lower = ContainerInfo(
            identifier: "L", name: "icloud", type: .cardDAV, defaultIncluded: true)
        let upper = ContainerInfo(
            identifier: "U", name: "ICLOUD", type: .cardDAV, defaultIncluded: true)
        let defaults = ContainerPicker.defaults(for: [lower, upper])
        XCTAssertEqual(Set(defaults), Set(["L", "U"]))
    }

    // MARK: - render

    func testRenderEmptyContainers() {
        let s = ContainerPicker.render([])
        XCTAssertTrue(s.contains("No iCloud Contacts containers visible"))
    }

    func testRenderListsContainersWithIdentifiers() {
        let s = ContainerPicker.render([iCloudContainer, googleContainer])
        XCTAssertTrue(s.contains("1. iCloud (1B7C9)"))
        XCTAssertTrue(s.contains("2. Google (F2C7B)"))
        XCTAssertTrue(s.contains("[default]"))
    }

    func testRenderMarksGoogleCardDAVAsCoveredByGcontacts() {
        let s = ContainerPicker.render([googleContainer])
        XCTAssertTrue(s.contains("[skipped — likely covered by gcontacts]"))
    }

    func testRenderMarksExchangeAsSkipped() {
        let s = ContainerPicker.render([exchangeContainer])
        XCTAssertTrue(s.contains("[skipped — Exchange]"))
    }

    // MARK: - parse: valid input

    func testParseEmptyInputReturnsDefaults() throws {
        let picked = try ContainerPicker.parse(
            input: "", containers: [iCloudContainer, googleContainer, onMyMacContainer])
        XCTAssertEqual(Set(picked), Set([iCloudContainer.identifier, onMyMacContainer.identifier]))
    }

    func testParseSingleNumber() throws {
        let picked = try ContainerPicker.parse(
            input: "1", containers: [iCloudContainer, googleContainer])
        XCTAssertEqual(picked, [iCloudContainer.identifier])
    }

    func testParseCommaSeparatedNumbers() throws {
        let picked = try ContainerPicker.parse(
            input: "1,3", containers: [iCloudContainer, googleContainer, onMyMacContainer])
        XCTAssertEqual(picked, [iCloudContainer.identifier, onMyMacContainer.identifier])
    }

    func testParseAcceptsWhitespaceAroundNumbers() throws {
        let picked = try ContainerPicker.parse(
            input: " 1 , 2 ", containers: [iCloudContainer, googleContainer])
        XCTAssertEqual(picked, [iCloudContainer.identifier, googleContainer.identifier])
    }

    // MARK: - parse: rejections

    func testParseRejectsNonNumeric() {
        XCTAssertThrowsError(try ContainerPicker.parse(
            input: "abc", containers: [iCloudContainer])) { err in
                guard case ContainerPickerError.invalidInput = err else {
                    return XCTFail("expected invalidInput, got \(err)")
                }
            }
    }

    func testParseRejectsOutOfRange() {
        XCTAssertThrowsError(try ContainerPicker.parse(
            input: "5", containers: [iCloudContainer])) { err in
                guard case ContainerPickerError.invalidInput = err else {
                    return XCTFail("expected invalidInput, got \(err)")
                }
            }
    }

    func testParseRejectsZero() {
        XCTAssertThrowsError(try ContainerPicker.parse(
            input: "0", containers: [iCloudContainer]))
    }

    func testParseRejectsDuplicateIndices() {
        XCTAssertThrowsError(try ContainerPicker.parse(
            input: "1,1", containers: [iCloudContainer, googleContainer])) { err in
                guard case ContainerPickerError.invalidInput = err else {
                    return XCTFail("expected invalidInput, got \(err)")
                }
            }
    }

    func testParseRejectsAllKeyword() {
        // Post-Codex-r1 D-JC3 — the `all` keyword conflicts with the
        // fail-closed-for-unknown-containers requirement.
        XCTAssertThrowsError(try ContainerPicker.parse(
            input: "all", containers: [iCloudContainer])) { err in
                guard case ContainerPickerError.invalidInput = err else {
                    return XCTFail("expected invalidInput, got \(err)")
                }
            }
        XCTAssertThrowsError(try ContainerPicker.parse(
            input: "ALL", containers: [iCloudContainer]))
    }

    func testParseRejectsEmptyContainerList() {
        XCTAssertThrowsError(try ContainerPicker.parse(input: "1", containers: [])) { err in
            guard case ContainerPickerError.noContainers = err else {
                return XCTFail("expected noContainers, got \(err)")
            }
        }
    }
}
