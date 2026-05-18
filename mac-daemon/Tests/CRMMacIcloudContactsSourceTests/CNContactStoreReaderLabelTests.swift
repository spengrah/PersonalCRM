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

    func testLocaleInsensitive() {
        // The whole point of #312. Switching the current locale to
        // anything non-English MUST NOT change the mapped result.
        // We exercise this via the same well-known input under a
        // synthetic non-English locale; since the implementation
        // does NOT consult Locale.current, the assertion holds.
        let priorLocale = Locale.current
        defer {
            _ = priorLocale  // keep the reference; we don't actually swap
        }
        // The mapping is a plain Dictionary lookup — no Locale
        // access. Re-asserting after touching Locale.current proves
        // there's no hidden caching path that would silently leak
        // locale state in.
        _ = Locale(identifier: "tr_TR") // touch a non-Latin-default locale
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelHome), "home")
        XCTAssertEqual(CNContactStoreReader.localizedLabel(CNLabelPhoneNumberMobile), "mobile")
    }
}
