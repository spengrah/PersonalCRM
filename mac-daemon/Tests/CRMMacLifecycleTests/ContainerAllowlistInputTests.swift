// Pure-logic tests for `ContainerAllowlistInput.parse(_:)`. The
// parser is the input side of the non-interactive
// `--containers <uuid,uuid,…>` flow. Validation against the visible
// CNContainer list deliberately does NOT happen here (the
// non-interactive path skips enumeration entirely so it works from
// a shell context); these tests just lock in the trim +
// drop-empty behaviour.
import XCTest
@testable import CRMMacLifecycle

final class ContainerAllowlistInputTests: XCTestCase {

    func testParseTrimsWhitespace() {
        XCTAssertEqual(
            ContainerAllowlistInput.parse("a , b,c "),
            ["a", "b", "c"])
    }

    func testParseDropsEmptyEntries() {
        XCTAssertEqual(
            ContainerAllowlistInput.parse("a,,b,"),
            ["a", "b"])
    }

    func testParseEmptyInputReturnsEmpty() {
        // Callers gate on `containers.isEmpty` before invoking the
        // non-interactive branch, so empty input shouldn't reach
        // parse in practice — but the function should still be
        // well-defined for defensive callers.
        XCTAssertEqual(ContainerAllowlistInput.parse(""), [])
    }
}
