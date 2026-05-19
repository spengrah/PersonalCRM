// Pure-function tests for the identity-coercion + argv-construction logic
// added with CRM_MAC_CODESIGN_IDENTITY support. The actual `codesign`
// shell-out is exercised by the opt-in integration tests
// (BundleAssemblyParityTests / BundleCodesignSealTests); this file
// covers the parts that don't need an actual codesign binary.
import XCTest
@testable import CRMMacSystem

final class ProductionExecutableAdapterTests: XCTestCase {
    // MARK: - signingIdentity coercion

    func testInitWithNilIdentityProducesNilSigningIdentity() {
        let adapter = ProductionExecutableAdapter(signingIdentity: nil)
        XCTAssertNil(adapter.signingIdentity)
    }

    func testInitWithEmptyIdentityProducesNilSigningIdentity() {
        let adapter = ProductionExecutableAdapter(signingIdentity: "")
        XCTAssertNil(adapter.signingIdentity)
    }

    func testInitWithWhitespaceIdentityProducesNilSigningIdentity() {
        let adapter = ProductionExecutableAdapter(signingIdentity: "   ")
        XCTAssertNil(adapter.signingIdentity)
    }

    func testInitWithDashIdentityProducesNilSigningIdentity() {
        // "-" is codesign's ad-hoc sentinel; treat it as "not configured"
        // so a user can `export CRM_MAC_CODESIGN_IDENTITY=-` to force
        // ad-hoc without unsetting the variable.
        let adapter = ProductionExecutableAdapter(signingIdentity: "-")
        XCTAssertNil(adapter.signingIdentity)
    }

    func testInitWithRealIdentityRetainsIt() {
        let adapter = ProductionExecutableAdapter(
            signingIdentity: "CRM Mac Local Code Signing")
        XCTAssertEqual(adapter.signingIdentity, "CRM Mac Local Code Signing")
    }

    func testInitWithPaddedIdentityTrimsWhitespace() {
        let adapter = ProductionExecutableAdapter(
            signingIdentity: "  CRM Mac Local Code Signing\n")
        XCTAssertEqual(adapter.signingIdentity, "CRM Mac Local Code Signing")
    }

    // MARK: - signingArguments

    func testAdhocSigningArgumentsOmitTimestampFlag() {
        let adapter = ProductionExecutableAdapter(signingIdentity: nil)
        XCTAssertEqual(
            adapter.signingArguments(identifier: "xyz.spengrah.crm-mac"),
            [
                "--sign", "-",
                "--identifier", "xyz.spengrah.crm-mac",
            ])
    }

    func testCertBackedSigningArgumentsAppendTimestampNone() {
        // --timestamp=none keeps the local-cert codesign call from
        // reaching out to Apple's TSA, which would fail for a self-
        // signed cert. The flag is deliberately conditional so the
        // ad-hoc path is unchanged.
        let adapter = ProductionExecutableAdapter(
            signingIdentity: "CRM Mac Local Code Signing")
        XCTAssertEqual(
            adapter.signingArguments(identifier: "xyz.spengrah.crm-mac"),
            [
                "--sign", "CRM Mac Local Code Signing",
                "--identifier", "xyz.spengrah.crm-mac",
                "--timestamp=none",
            ])
    }
}
