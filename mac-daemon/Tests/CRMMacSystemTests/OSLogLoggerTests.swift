import XCTest
@testable import CRMMacSystem
import CRMMacCore

final class OSLogLoggerTests: XCTestCase {
    func testDoesNotCrashAcrossAllLevels() {
        // Smoke test — we can't intercept os_log output from XCTest,
        // but we can prove the wrapper handles each level without
        // crashing. The compose() unit test below covers the privacy
        // semantics.
        let logger = OSLogLogger(category: "tests")
        logger.debug("d", metadata: ["k": .public("v")])
        logger.info("i", metadata: ["k": .private("secret-uuid")])
        logger.warning("w")
        logger.error("e")
    }

    func testComposeOrdersMetadataAndRedactsPrivate() {
        let result = OSLogLogger.compose(
            message: "hello",
            metadata: [
                "z_pub": .public("v1"),
                "a_priv": .private("v2"),
            ])
        // Keys are sorted alphabetically so the output is deterministic.
        XCTAssertEqual(result, "hello a_priv=<redacted> z_pub=v1")
    }

    func testComposeEmptyMetadataReturnsMessageOnly() {
        XCTAssertEqual(OSLogLogger.compose(message: "x", metadata: [:]), "x")
    }

    func testComposePrivateValuesAreRedacted() {
        let result = OSLogLogger.compose(
            message: "request",
            metadata: ["uuid": .private("11111111-2222-3333-4444-555555555555")])
        XCTAssertFalse(result.contains("11111111"))
        XCTAssertTrue(result.contains("<redacted>"))
    }
}
