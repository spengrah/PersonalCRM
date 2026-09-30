import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class AnarlogConfigureFlowTests: XCTestCase {
    func testApplyAllFieldsAndPreservesUnrelatedConfig() throws {
        var config = try configuredConfig()
        let request = AnarlogConfigureRequest(
            operatorPersonID: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
            cliPath: "/opt/anarlog/bin/anarlog",
            deletionCap: 7)

        try AnarlogConfigureFlow.apply(request, to: &config)

        var expected = AnarlogConfig(
            humansEnabled: true,
            sessionsEnabled: false)
        try expected.setOperatorPersonID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
        try expected.setCLIPath("/opt/anarlog/bin/anarlog")
        try expected.setDeletionCap(7)
        XCTAssertEqual(config, expected)
    }

    func testApplyOperatorPersonIDAloneChangesOnlyThatField() throws {
        var config = try configuredConfig()
        let original = config

        try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(
                operatorPersonID: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
                cliPath: nil,
                deletionCap: nil),
            to: &config)

        var expected = original
        try expected.setOperatorPersonID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
        XCTAssertEqual(config, expected)
    }

    func testApplyCLIPathAloneChangesOnlyThatField() throws {
        var config = try configuredConfig()
        let original = config

        try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(
                operatorPersonID: nil,
                cliPath: "/opt/anarlog/bin/anarlog",
                deletionCap: nil),
            to: &config)

        var expected = original
        try expected.setCLIPath("/opt/anarlog/bin/anarlog")
        XCTAssertEqual(config, expected)
    }

    func testApplyDeletionCapAloneChangesOnlyThatField() throws {
        var config = try configuredConfig()

        try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(
                operatorPersonID: nil,
                cliPath: nil,
                deletionCap: 7),
            to: &config)

        var expected = try configuredConfig()
        try expected.setDeletionCap(7)
        XCTAssertEqual(config, expected)
    }

    func testApplyAllNilRequestLeavesConfigUnchanged() throws {
        var config = try configuredConfig()
        let original = config

        try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(operatorPersonID: nil, cliPath: nil, deletionCap: nil),
            to: &config)

        XCTAssertEqual(config, original)
    }

    func testApplyFailureIsAtomicWhenDeletionCapIsNegative() throws {
        var config = try configuredConfig()
        let original = config

        XCTAssertThrowsError(try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(
                operatorPersonID: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
                cliPath: "/new/anarlog",
                deletionCap: -1),
            to: &config)) { error in
                XCTAssertEqual(error as? AnarlogConfigError, .negativeDeletionCap(-1))
            }

        XCTAssertEqual(config, original)
    }

    func testApplyInvalidOperatorPersonIDLeavesConfigUnchanged() throws {
        var config = try configuredConfig()
        let original = config

        XCTAssertThrowsError(try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(
                operatorPersonID: "not-a-uuid",
                cliPath: nil,
                deletionCap: nil),
            to: &config)) { error in
                XCTAssertEqual(error as? AnarlogConfigError, .operatorPersonIDNotUUID("not-a-uuid"))
            }

        XCTAssertEqual(config, original)
    }

    func testApplyRelativeCLIPathLeavesConfigUnchanged() throws {
        var config = try configuredConfig()
        let original = config

        XCTAssertThrowsError(try AnarlogConfigureFlow.apply(
            AnarlogConfigureRequest(
                operatorPersonID: nil,
                cliPath: "relative/anarlog",
                deletionCap: nil),
            to: &config)) { error in
                XCTAssertEqual(error as? AnarlogConfigError, .cliPathNotAbsolute("relative/anarlog"))
            }

        XCTAssertEqual(config, original)
    }

    func testMessageReturnsExactTextForEachConfigError() {
        XCTAssertEqual(
            AnarlogConfigureFlow.message(for: .operatorPersonIDNotUUID("raw-value")),
            "anarlog operator person id must be a UUID, got: raw-value")
        XCTAssertEqual(
            AnarlogConfigureFlow.message(for: .cliPathNotAbsolute("relative/path")),
            "anarlog cli path must be absolute, got: relative/path")
        XCTAssertEqual(
            AnarlogConfigureFlow.message(for: .negativeDeletionCap(-1)),
            "anarlog deletion cap must be non-negative, got: -1")
    }

    func testSummaryLinesForConfiguredAndDefaultValues() throws {
        var configured = AnarlogConfig(
            humansEnabled: true,
            sessionsEnabled: false)
        try configured.setOperatorPersonID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
        try configured.setCLIPath("/opt/anarlog/bin/anarlog")
        try configured.setDeletionCap(7)
        XCTAssertEqual(AnarlogConfigureFlow.summaryLines(configured), [
            "  operator_person_id: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
            "  cli_path:           /opt/anarlog/bin/anarlog",
            "  deletion_cap:       7",
        ])

        let defaults = AnarlogConfig()
        XCTAssertEqual(AnarlogConfigureFlow.summaryLines(defaults), [
            "  operator_person_id: (unset)",
            "  cli_path:           (default search)",
            "  deletion_cap:       5",
        ])
    }

    private func configuredConfig() throws -> AnarlogConfig {
        var config = AnarlogConfig(
            humansEnabled: true,
            sessionsEnabled: false)
        try config.setOperatorPersonID("11111111-2222-3333-4444-555555555555")
        try config.setCLIPath("/existing/anarlog")
        try config.setDeletionCap(3)
        return config
    }
}
