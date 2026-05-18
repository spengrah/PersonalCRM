// CNContactStoreReaderLabelTests cover the locale-stable label
// normalization. Pure-logic; no CNContactStore access required.
//
// The old implementation called
// `CNLabeledValue<NSString>.localizedString(forLabel:)`, which is
// locale-sensitive. Because the label string feeds the JCS+SHA-256
// canonicalization that produces the content hash (and thus the
// envelope's source_id), a locale flip would re-hash every contact
// and break Pi-side `/known-ids` parity. These tests pin the new
// mapping so a future regression that re-introduces locale lookups
// would fail here even on an `en_US` runner.
import XCTest
import Contacts
@testable import CRMMacIcloudContactsSource

final class CNContactStoreReaderLabelTests: XCTestCase {

    func testNilOrEmptyReturnsNil() {
        XCTAssertNil(CNContactStoreReader.localizedLabel(nil))
        XCTAssertNil(CNContactStoreReader.localizedLabel(""))
    }

    func testWellKnownGenericLabels() {
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelHome), "home")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelWork), "work")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelSchool), "school")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelOther), "other")
    }

    func testWellKnownPhoneLabels() {
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberiPhone), "iphone")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberMobile), "mobile")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberMain), "main")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberHomeFax), "home fax")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberWorkFax), "work fax")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberOtherFax), "other fax")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberPager), "pager")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberAppleWatch), "apple watch")
    }

    func testWellKnownEmailAndUrlLabels() {
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelEmailiCloud), "icloud")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelURLAddressHomePage), "homepage")
    }

    func testWellKnownDateLabel() {
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelDateAnniversary), "anniversary")
    }

    func testCustomLabelLowercases() {
        // User-defined labels (e.g. "Mom") aren't in the map and
        // don't have Apple's wrapper. They pass through with only
        // the lowercase step.
        XCTAssertEqual(CNContactStoreReader.localizedLabel("Mom"), "mom")
        XCTAssertEqual(CNContactStoreReader.localizedLabel("Spencer's iPhone"), "spencer's iphone")
    }

    func testUnknownWrappedLabelStripsWrapper() {
        // Defensive: if Apple ships a new CNLabel* constant we
        // haven't enumerated, strip the `_$!<…>!$_` wrapper rather
        // than emit it raw on the wire.
        XCTAssertEqual(
            CNContactStoreReader.localizedLabel("_$!<NewLabel>!$_"),
            "newlabel")
    }

    func testSourceDoesNotCallLocalizedString() throws {
        // Structural guarantee for the locale-stability invariant:
        // `CNContactStoreReader.swift` must not invoke
        // `CNLabeledValue.localizedString(forLabel:)`. The mapping is
        // a pure dictionary lookup with no Locale access, so if the
        // source contains no call to `localizedString(forLabel:)`,
        // the function is locale-insensitive by construction. Greps
        // the source rather than trying to swap Locale.current
        // (which AppKit/Foundation makes effectively impossible
        // mid-process anyway).
        let sourceURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()           // Tests/CRMMacIcloudContactsSourceTests
            .deletingLastPathComponent()           // Tests
            .deletingLastPathComponent()           // mac-daemon
            .appendingPathComponent("Sources/CRMMacIcloudContactsSource/CNContactStoreReader.swift")
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        XCTAssertFalse(
            source.contains("localizedString(forLabel:"),
            "CNContactStoreReader.swift must not call localizedString(forLabel:) — that call is locale-sensitive and breaks /known-ids hash parity across locales (#312).")
    }
}
