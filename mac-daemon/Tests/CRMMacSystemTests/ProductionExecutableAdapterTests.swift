// Pure-function tests for the identity-coercion + argv-construction logic
// added with CRM_MAC_CODESIGN_IDENTITY support. The actual `codesign`
// shell-out is exercised by the opt-in integration tests
// (BundleAssemblyParityTests / BundleCodesignSealTests); this file
// covers the parts that don't need an actual codesign binary.
import XCTest
@testable import CRMMacSystem
import CRMMacLifecycle

final class ProductionExecutableAdapterTests: XCTestCase {
    // POSIX env var read by the default initializer expression. Each
    // test that exercises the default initializer must guarantee its
    // own setenv/unsetenv state — Swift's default-parameter values are
    // evaluated at the call site, so the live process env is observable.
    private let envVar = "CRM_MAC_CODESIGN_IDENTITY"

    override func setUpWithError() throws {
        // Belt-and-suspenders: explicitly clear the env var before each
        // test so a developer's shell can't perturb the no-args
        // initializer cases below.
        unsetenv(envVar)
    }

    override func tearDownWithError() throws {
        unsetenv(envVar)
    }

    // MARK: - signingIdentity coercion (explicit-arg path)

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

    // MARK: - default-initializer reads CRM_MAC_CODESIGN_IDENTITY

    func testDefaultInitWithUnsetEnvProducesNil() {
        // setUpWithError already unsetenv'd; verify the no-args path.
        let adapter = ProductionExecutableAdapter()
        XCTAssertNil(adapter.signingIdentity)
    }

    func testDefaultInitWithEmptyEnvProducesNil() {
        setenv(envVar, "", 1)
        let adapter = ProductionExecutableAdapter()
        XCTAssertNil(adapter.signingIdentity)
    }

    func testDefaultInitReadsEnvVarWhenSet() {
        setenv(envVar, "Some Local Code Signing", 1)
        let adapter = ProductionExecutableAdapter()
        XCTAssertEqual(adapter.signingIdentity, "Some Local Code Signing")
    }

    // MARK: - signingArguments

    func testAdhocSigningArgumentsOmitTimestampFlag() {
        let adapter = ProductionExecutableAdapter(signingIdentity: nil)
        XCTAssertEqual(
            adapter.signingArguments(identifier: Daemon.label),
            [
                "--sign", "-",
                "--identifier", Daemon.label,
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
            adapter.signingArguments(identifier: Daemon.label),
            [
                "--sign", "CRM Mac Local Code Signing",
                "--identifier", Daemon.label,
                "--timestamp=none",
            ])
    }
}
