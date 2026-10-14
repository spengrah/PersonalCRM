// The rescan-messages group pass, observed at the entry point over a
// messages-database fixture with the Pi faked at PiClient's transport boundary.
import XCTest
import Foundation
import CRMMacPiClient

final class GroupPassTests: XCTestCase {
    private let base = Date(timeIntervalSince1970: 1_767_225_600) // 2026-01-01 UTC
    private let chat = "iMessage;+;groupX"

    private func at(_ offset: TimeInterval) -> Date { base.addingTimeInterval(offset) }
    private func window(_ start: TimeInterval, _ end: TimeInterval) -> (start: Date, end: Date) {
        (at(start), at(end))
    }

    private func inbound(
        _ guid: String, _ offset: TimeInterval, chat: String? = nil, handle: String = "+15550001111",
        text: String? = "hello group"
    ) -> SourceRow {
        SourceRow(guid: guid, text: text, attributedBody: nil,
                  chat: chat ?? self.chat, handle: handle, sentAt: at(offset))
    }

    private func outbound(_ guid: String, _ offset: TimeInterval, chat: String? = nil) -> SourceRow {
        SourceRow(guid: guid, text: "my words", attributedBody: nil,
                  chat: chat ?? self.chat, isFromMe: true, sentAt: at(offset))
    }

    private func guids(_ pi: FakePi) -> [String] { pi.groupItems.map { $0["guid"] as! String } }
    private func ops(_ pi: FakePi) -> [String] { pi.groupItems.map { $0["op"] as! String } }

    func testInWindowInboundMessageFromANonContactIsSubmittedWithItsRawHandleAndBody() async throws {
        let fixture = try RescanFixture(source: [inbound("m1", 150)])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.groupItems.count, 1)
        let item = try XCTUnwrap(pi.groupItems.first)
        XCTAssertEqual(item["op"] as? String, "add_group_message")
        XCTAssertEqual(item["guid"] as? String, "m1")
        XCTAssertEqual(item["chat_guid"] as? String, chat)
        XCTAssertEqual(item["sender"] as? String, "+15550001111")
        XCTAssertEqual(item["direction"] as? String, "inbound")
        XCTAssertEqual(item["sent_at"] as? String, "2026-01-01T00:02:30Z")
        XCTAssertEqual(item["body"] as? String, "hello group")
    }

    func testInWindowOutboundMessageIsSubmittedWithANullSenderFollowedByItsClear() async throws {
        let fixture = try RescanFixture(source: [outbound("m1", 150)])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]],
                        groupRows: ["m1": PiGroupRow(chat: chat, outbound: true, attributed: true)])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(ops(pi), ["add_group_message", "clear_outbound_peer"])
        XCTAssertEqual(guids(pi), ["m1", "m1"])
        let add = pi.groupItems[0]
        XCTAssertEqual(add["direction"] as? String, "outbound")
        XCTAssertTrue(add["sender"] is NSNull, "the user's own message has a null sender")
        XCTAssertEqual(pi.groupRow("m1")?.attributed, false)
    }

    func testMessagesAtAWindowsStartAndEndAreSubmittedAndOneSecondOutsideAreNot() async throws {
        let fixture = try RescanFixture(source: [
            inbound("before", 99), inbound("at-start", 100), inbound("inside", 150),
            inbound("at-end", 200), inbound("after", 201),
        ])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(guids(pi), ["at-start", "inside", "at-end"])
    }

    func testAMessageIsSelectedByItsSentTimeAsEncodedInWholeSeconds() async throws {
        let fixture = try RescanFixture(source: [
            inbound("just-before", 99.5), inbound("start-second", 100.5),
            inbound("end-second", 200.5), inbound("just-after", 201.5),
        ])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(guids(pi), ["start-second", "end-second"])
        XCTAssertEqual(pi.groupItems.map { $0["sent_at"] as? String },
                       ["2026-01-01T00:01:40Z", "2026-01-01T00:03:20Z"])
    }

    func testAMessageOfAChatTheListingDoesNotNameIsNotSubmitted() async throws {
        let fixture = try RescanFixture(source: [
            inbound("named", 150), inbound("unnamed", 150, chat: "iMessage;+;groupZ"),
        ])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(guids(pi), ["named"])
    }

    func testAMessageInNoneOfAChatsSeveralWindowsIsNotSubmitted() async throws {
        let fixture = try RescanFixture(source: [
            inbound("in-first", 150), inbound("between", 300), inbound("in-second", 450),
        ])
        let pi = FakePi(groupChats: [chat: [window(100, 200), window(400, 500)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(guids(pi), ["in-first", "in-second"])
    }

    func testAListingSpanningTwoPagesHasBothPagesRead() async throws {
        let chats = ["iMessage;+;a", "iMessage;+;b", "iMessage;+;c"]
        let fixture = try RescanFixture(source: chats.enumerated().map { i, c in
            inbound("m\(i)", 150, chat: c)
        })
        let pi = FakePi(groupChats: Dictionary(uniqueKeysWithValues: chats.map { ($0, [window(100, 200)]) }),
                        groupPageCap: 2)

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.groupListRequests, 2)
        XCTAssertEqual(Set(guids(pi)), ["m0", "m1", "m2"])
        XCTAssertEqual(result.report["group_chats"], "3")
    }

    func testMoreThan500SelectedItemsGoInRequestsOfAtMost500Items() async throws {
        let fixture = try RescanFixture(source: (0...500).map { inbound(String(format: "m%04d", $0), 100 + Double($0)) })
        let pi = FakePi(groupChats: [chat: [window(100, 700)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertGreaterThan(pi.groupRequests.count, 1)
        XCTAssertTrue(pi.groupRequests.allSatisfy { $0.count <= 500 })
        XCTAssertEqual(pi.groupItems.count, 501)
        XCTAssertEqual(result.report["group.added"], "501")
    }

    func testFewerThan500ItemsEncodingPast8MiBGoInMoreThanOneRequestEachUnder8MiB() async throws {
        let body = String(repeating: "a", count: 500_000)
        let fixture = try RescanFixture(source: (0..<20).map { inbound("m\($0)", 100 + Double($0), text: body) })
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        // The fake Pi asserts each request body is under 8 MiB.
        XCTAssertEqual(result.exitCode, 0)
        XCTAssertGreaterThan(pi.groupRequests.count, 1)
        XCTAssertEqual(pi.groupItems.count, 20)
        XCTAssertEqual(result.report["group.added"], "20")
    }

    func testAMessageWithNoBodyIsSubmittedWithANullBody() async throws {
        let fixture = try RescanFixture(source: [inbound("m1", 150, text: nil)])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        let item = try XCTUnwrap(pi.groupItems.first)
        XCTAssertTrue(item["body"] is NSNull)
    }

    /// Chat X: a new inbound message, an inbound one the Pi holds, and an
    /// outbound one the Pi holds with a member attribution. Chat Y: an outbound
    /// message, in a chat the Pi stops serving windows for after the listing.
    private func mixedGroupRun() throws -> (RescanFixture, FakePi) {
        let chatY = "iMessage;+;groupY"
        let fixture = try RescanFixture(source: [
            inbound("new-in", 120, handle: "secret-handle@example.test", text: "secret group words"),
            inbound("held-in", 130), outbound("held-out", 140),
            outbound("late-out", 150, chat: chatY),
        ])
        let pi = FakePi(
            groupChats: [chat: [window(100, 200)], chatY: [window(100, 200)]],
            groupRows: [
                "held-in": PiGroupRow(chat: chat, outbound: false, attributed: true),
                "held-out": PiGroupRow(chat: chat, outbound: true, attributed: true),
            ])
        pi.afterGroupListing = { [chatY] chats in chats[chatY] = [] }
        return (fixture, pi)
    }

    func testThePisAnswersAreCountedUnderTheGroupLinesAndNoContentIsReported() async throws {
        let (fixture, pi) = try mixedGroupRun()
        let before = try fileTree(fixture.home)

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        let r = result.report
        XCTAssertEqual(r["group_chats"], "2")
        XCTAssertEqual(r["group.added"], "1")
        XCTAssertEqual(r["group.unchanged"], "2")
        XCTAssertEqual(r["group.outside_window"], "1")
        XCTAssertEqual(r["group.cleared"], "1")
        XCTAssertEqual(r["group.unknown"], "1")
        // The body pass's report lines stay.
        XCTAssertEqual(r["listed"], "0")
        for secret in ["secret", "my words", "hello group"] {
            XCTAssertFalse(result.output.stdoutText.contains(secret))
            XCTAssertFalse(result.output.stderrText.contains(secret))
        }
        XCTAssertEqual(try fileTree(fixture.home), before, "the run writes no file")
    }

    func testASecondRunAgainstThePiStateTheFirstLeftReportsNoAddAndNoClear() async throws {
        let fixture = try RescanFixture(source: [inbound("new-in", 120), outbound("held-out", 140)])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]],
                        groupRows: ["held-out": PiGroupRow(chat: chat, outbound: true, attributed: true)])
        let first = await fixture.run(pi: pi)
        XCTAssertEqual(first.report["group.added"], "1")
        XCTAssertEqual(first.report["group.cleared"], "1")

        let second = await fixture.run(pi: pi)

        XCTAssertEqual(second.exitCode, 0)
        XCTAssertEqual(second.report["group.added"], "0")
        XCTAssertEqual(second.report["group.cleared"], "0")
    }

    func testTheGroupChatsListingRejectedEndsTheRunNonZeroWithNothingSubmittedAndNoBodyPass() async throws {
        let fixture = try RescanFixture(source: [inbound("m1", 150), SourceRow(guid: "g1", text: "body", attributedBody: nil)])
        let pi = FakePi(rows: ["g1": nil], failure: .groupListingRejected,
                        groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(result.output.stdoutText.isEmpty, "no report")
        XCTAssertTrue(pi.groupRequests.isEmpty)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(pi.listRequests, 0, "the body pass did not run")
    }

    func testTheGroupChatsListingUnreachableEndsTheRunNonZeroWithNothingSubmittedAndNoBodyPass() async throws {
        let fixture = try RescanFixture(source: [inbound("m1", 150), SourceRow(guid: "g1", text: "body", attributedBody: nil)])
        let pi = FakePi(rows: ["g1": nil], failure: .groupListingUnreachable,
                        groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(result.output.stdoutText.isEmpty, "no report")
        XCTAssertTrue(pi.groupRequests.isEmpty)
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(pi.listRequests, 0, "the body pass did not run")
    }

    private func manyMessagesAndABodylessRow() throws -> RescanFixture {
        try RescanFixture(source: (0...500).map { inbound(String(format: "m%04d", $0), 100 + Double($0)) }
                          + [SourceRow(guid: "g1", text: "body", attributedBody: nil)])
    }

    func testAnEnrichmentSubmissionRejectedEndsTheRunNonZeroWithNoFurtherSubmissionAndNoBodyPass() async throws {
        let fixture = try manyMessagesAndABodylessRow()
        let pi = FakePi(rows: ["g1": nil], failure: .groupSubmission(1),
                        groupChats: [chat: [window(100, 700)]])

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(result.output.stdoutText.isEmpty, "no report")
        XCTAssertEqual(pi.groupRequests.count, 1, "a second request would have been needed")
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(pi.listRequests, 0, "the body pass did not run")
    }

    func testAnEnrichmentSubmissionUnreachableEndsTheRunNonZeroWithNoFurtherSubmissionAndNoBodyPass() async throws {
        let fixture = try manyMessagesAndABodylessRow()
        let pi = FakePi(rows: ["g1": nil], failure: .groupSubmissionUnreachable,
                        groupChats: [chat: [window(100, 700)]])

        let result = await fixture.run(pi: pi)

        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(result.output.stdoutText.isEmpty, "no report")
        XCTAssertEqual(pi.groupRequests.count, 1, "a second request would have been needed")
        XCTAssertTrue(pi.fillRequests.isEmpty)
        XCTAssertEqual(pi.listRequests, 0, "the body pass did not run")
    }

    func testARowTheGroupPassAddsWithoutABodyIsClassifiedByTheSameRunsBodyPass() async throws {
        let fixture = try RescanFixture(source: [inbound("m1", 150, text: nil)])
        let pi = FakePi(groupChats: [chat: [window(100, 200)]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(result.report["group.added"], "1")
        XCTAssertEqual(result.report["listed"], "1")
        XCTAssertEqual(result.report["by_nature.none_stored"], "1")
    }

    private func fileTree(_ root: URL) throws -> [String] {
        let enumerator = FileManager.default.enumerator(atPath: root.path)
        return (enumerator?.allObjects as? [String] ?? []).sorted()
    }
}
