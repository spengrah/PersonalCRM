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

    // MARK: - parseIdentifier (post-sign verification)

    func testParseIdentifierFindsMarkerLine() {
        let output = """
        Executable=/some/path
        Identifier=xyz.spengrah.crm-mac
        Format=app bundle
        """
        XCTAssertEqual(
            ProductionExecutableAdapter.parseIdentifier(from: output),
            "xyz.spengrah.crm-mac")
    }

    func testParseIdentifierStripsTrailingCRAndWhitespace() {
        // codesign output piped through a tty translation layer can
        // arrive CRLF-terminated; a brittle parser would compare
        // "xyz.spengrah.crm-mac\r" against the bare identifier and
        // throw a confusing mismatch error.
        let output = "Identifier=xyz.spengrah.crm-mac  \r\nFormat=app bundle"
        XCTAssertEqual(
            ProductionExecutableAdapter.parseIdentifier(from: output),
            "xyz.spengrah.crm-mac")
    }

    func testParseIdentifierReturnsNilWhenMarkerAbsent() {
        // Defends the install-time verifier's "no Identifier line"
        // error branch — if a future codesign refactor stops emitting
        // the marker, the install loudly fails instead of silently
        // skipping the identifier-match check.
        let output = "Executable=/some/path\nFormat=app bundle"
        XCTAssertNil(ProductionExecutableAdapter.parseIdentifier(from: output))
    }

    // MARK: - parseDesignatedRequirement (post-sign verification)

    func testParseDesignatedRequirementMatchesCertLeafLine() {
        let output = """
        Executable=/some/path
        designated => identifier "xyz.spengrah.crm-mac" and certificate leaf = H"deadbeef"
        """
        XCTAssertEqual(
            ProductionExecutableAdapter.parseDesignatedRequirement(from: output),
            "identifier \"xyz.spengrah.crm-mac\" and certificate leaf = H\"deadbeef\"")
    }

    func testParseDesignatedRequirementMatchesHashCommentVariant() {
        // Older / newer macOS codesign variants emit the requirement
        // line prefixed with "# " — accept both.
        let output = """
        Executable=/some/path
        # designated => cdhash H"abcd1234"
        """
        XCTAssertEqual(
            ProductionExecutableAdapter.parseDesignatedRequirement(from: output),
            "cdhash H\"abcd1234\"")
    }

    func testParseDesignatedRequirementDetectsCdhashAnchor() {
        // This is the cert-mode regression guard's payload: if
        // codesign silently produces a cdhash DR despite an identity
        // being given, the verifier must throw. Parser returns the
        // cdhash-anchored string; the verifier's `.contains("cdhash")`
        // check catches it.
        let output = "designated => cdhash H\"abcd1234\""
        let dr = ProductionExecutableAdapter.parseDesignatedRequirement(from: output)
        XCTAssertTrue(dr.contains("cdhash"),
                      "parser must surface cdhash so verifier can reject it")
    }

    func testParseDesignatedRequirementReturnsEmptyWhenAbsent() {
        // Defends the "could not parse designated requirement" throw —
        // an empty return would otherwise silently bypass the
        // cdhash-rejection guard in cert mode.
        let output = "Executable=/some/path\nFormat=app bundle"
        XCTAssertTrue(
            ProductionExecutableAdapter.parseDesignatedRequirement(from: output).isEmpty)
    }
}
