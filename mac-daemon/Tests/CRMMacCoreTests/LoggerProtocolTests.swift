import XCTest
@testable import CRMMacCore

final class LoggerProtocolTests: XCTestCase {
    func testNoopLoggerDoesNotCrash() {
        let logger = NoopLogger()
        logger.debug("debug message")
        logger.info("info message", metadata: ["key": .public("value")])
        logger.warning("warn", metadata: ["uuid": .private("11111111-2222-3333-4444-555555555555")])
        logger.error("err")
    }

    func testLogValueStringValueReturnsRaw() {
        XCTAssertEqual(LogValue.public("a").stringValue, "a")
        XCTAssertEqual(LogValue.private("b").stringValue, "b")
    }
}
