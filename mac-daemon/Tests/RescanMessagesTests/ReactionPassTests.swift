// The rescan-messages reaction pass, observed through the run entry over a
// messages-database fixture with the Pi faked at PiClient's transport
// boundary. Synthetic guids only.
import XCTest
import Foundation
import CRMMacPiClient

final class ReactionPassTests: XCTestCase {
    private let target = "AAAAAAAA-1111-2222-3333-444444444444"

    func testAListedReactionIsSubmittedWithItsFacts() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "love", text: "Loved a message", associatedMessageType: 2000,
                      associatedGUID: "p:0/\(target)"),
        ])
        let pi = FakePi(rows: ["love": "Loved a message"])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        let item = try XCTUnwrap(pi.reactionItems.first)
        XCTAssertEqual(pi.reactionItems.count, 1)
        XCTAssertEqual(item["guid"] as? String, "love")
        let reaction = try XCTUnwrap(item["reaction"] as? [String: Any])
        XCTAssertEqual(Set(reaction.keys), ["kind", "target_key", "removal"])
        XCTAssertEqual(reaction["kind"] as? String, "love")
        XCTAssertEqual(reaction["target_key"] as? String, target)
        XCTAssertEqual(reaction["removal"] as? Bool, false)
        XCTAssertEqual(result.report["reaction.marked"], "1")
    }

    func testAListedRemovalIsSubmittedWithItsKindEmojiTargetAndRemovalTrue() async throws {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "unparty", text: "Removed a reaction", associatedMessageType: 3006,
                      associatedGUID: "bp:\(target)", emoji: "🎉"),
        ])
        let pi = FakePi(rows: ["unparty": "Removed a reaction"])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.reactionItems.map { $0["guid"] as? String }, ["unparty"])
        let reaction = try XCTUnwrap(pi.reactionItems.first?["reaction"] as? [String: Any])
        XCTAssertEqual(reaction["kind"] as? String, "emoji")
        XCTAssertEqual(reaction["emoji"] as? String, "🎉")
        XCTAssertEqual(reaction["target_key"] as? String, target)
        XCTAssertEqual(reaction["removal"] as? Bool, true)
        XCTAssertEqual(result.report["reaction.marked"], "1")
    }

    /// A reaction, a non-reaction, an unrecognized type, and a guid the
    /// messages database does not hold, all listed by the Pi.
    private func mixedFixture() throws -> (RescanFixture, FakePi) {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "laugh", text: "secret laughed words", associatedMessageType: 2003,
                      associatedGUID: "p:0/\(target)"),
            SourceRow(guid: "plain", text: "secret plain words"),
            SourceRow(guid: "unrecognized", text: "secret future words", associatedMessageType: 2008,
                      associatedGUID: "p:0/\(target)"),
        ])
        let pi = FakePi(rows: ["laugh": "secret laughed words", "plain": "secret plain words",
                               "unrecognized": "secret future words", "absent": "secret absent words"])
        return (fixture, pi)
    }

    func testOnlyTheListedReactionIsSubmittedAndEveryOtherListedRowIsClassified() async throws {
        let (fixture, pi) = try mixedFixture()

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.reactionItems.map { $0["guid"] as? String }, ["laugh"])
        XCTAssertNil(pi.facts(of: "plain"))
        XCTAssertNil(pi.facts(of: "unrecognized"))
        XCTAssertNil(pi.facts(of: "absent"))
        XCTAssertEqual(result.report["reaction_listed"], "4")
        XCTAssertEqual(result.report["reaction.marked"], "1")
        XCTAssertEqual(result.report["reaction.not_reaction"], "2")
        XCTAssertEqual(result.report["reaction.unavailable"], "1")
    }

    func testASecondRunAgainstThePiStateTheFirstLeftSubmitsNothing() async throws {
        let (fixture, pi) = try mixedFixture()
        _ = await fixture.run(pi: pi)
        let submittedByFirst = pi.reactionRequests.count

        let second = await fixture.run(pi: pi)

        XCTAssertEqual(second.exitCode, 0)
        XCTAssertEqual(pi.reactionRequests.count, submittedByFirst, "second run submits nothing")
        XCTAssertEqual(second.report["reaction_listed"], "3")
        XCTAssertEqual(second.report["reaction.marked"], "0")
        XCTAssertEqual(second.report["reaction.not_reaction"], "2")
        XCTAssertEqual(second.report["reaction.unavailable"], "1")
    }

    func testTheOutputHoldsOnlyCountsAndNoMessageContent() async throws {
        let (fixture, pi) = try mixedFixture()

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        for line in result.output.stdoutText.split(separator: "\n") {
            let parts = line.split(separator: ":", maxSplits: 1)
            XCTAssertEqual(parts.count, 2, "\(line) is a key: count line")
            XCTAssertNotNil(Int(parts.last?.trimmingCharacters(in: .whitespaces) ?? ""), "\(line) holds a count")
        }
        for content in ["secret", target, "laugh", "plain", "unrecognized", "absent"] {
            XCTAssertFalse(result.output.stdoutText.contains(content), content)
            XCTAssertFalse(result.output.stderrText.contains(content), content)
        }
    }

    func testTheRunOpensTheMessagesDatabaseReadOnlyAndWritesNoFile() async throws {
        let (fixture, pi) = try mixedFixture()
        let chatDB = fixture.home.appendingPathComponent("Library/Messages/chat.db")
        try FileManager.default.setAttributes([.posixPermissions: 0o444], ofItemAtPath: chatDB.path)
        let bytesBefore = try Data(contentsOf: chatDB)
        let treeBefore = try fileTree(fixture.home)

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0, "a read-only messages database is enough")
        XCTAssertEqual(pi.reactionItems.count, 1)
        XCTAssertEqual(try Data(contentsOf: chatDB), bytesBefore)
        XCTAssertEqual(try fileTree(fixture.home), treeBefore)
    }

    func testAListingSpanningTwoPagesHasEveryListedReactionSubmitted() async throws {
        let guids = (0..<5).map { "love-\($0)" }
        let fixture = try RescanFixture(source: guids.map {
            SourceRow(guid: $0, text: "Loved a message", associatedMessageType: 2000,
                      associatedGUID: "p:0/\(target)")
        })
        let pi = FakePi(rows: Dictionary(uniqueKeysWithValues: guids.map { ($0, "Loved a message") }),
                        pageCap: 2)

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(pi.reactionListRequests, 3)
        XCTAssertEqual(pi.reactionItems.compactMap { $0["guid"] as? String }.sorted(), guids)
        XCTAssertEqual(result.report["reaction.marked"], "5")
    }

    func testAReactionTheGroupPassAddsIsMarkedByTheSameRunAfterTheBodyPass() async throws {
        let sentAt = Date(timeIntervalSince1970: 1_767_225_700)
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "group-love", text: "Loved a message", associatedMessageType: 2000,
                      associatedGUID: "p:0/\(target)", chat: "group-1", handle: "+15552220000",
                      sentAt: sentAt),
            SourceRow(guid: "bodyless", text: nil),
        ])
        let pi = FakePi(rows: ["bodyless": nil],
                        groupChats: ["group-1": [(start: sentAt.addingTimeInterval(-60),
                                                  end: sentAt.addingTimeInterval(60))]])

        let result = await fixture.run(pi: pi)

        XCTAssertEqual(result.exitCode, 0)
        XCTAssertEqual(result.report["group.added"], "1")
        XCTAssertEqual(result.report["by_nature.none_stored"], "1")
        XCTAssertEqual(pi.reactionItems.map { $0["guid"] as? String }, ["group-love"])
        XCTAssertEqual(result.report["reaction.marked"], "1")
    }

    // MARK: - a Pi that rejects or cannot be reached ends the run non-zero

    private func failingRun(_ failure: FakePi.Failure) async throws -> (RunResult, FakePi) {
        let fixture = try RescanFixture(source: [
            SourceRow(guid: "love", text: "Loved a message", associatedMessageType: 2000,
                      associatedGUID: "p:0/\(target)"),
        ])
        let pi = FakePi(rows: ["love": "Loved a message"], failure: failure)
        return (await fixture.run(pi: pi), pi)
    }

    func testARejectedReactionListingEndsTheRunNonZeroWithNoReport() async throws {
        let (result, pi) = try await failingRun(.reactionListingRejected)
        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(pi.reactionRequests.isEmpty)
        XCTAssertTrue(result.output.stdoutText.isEmpty)
    }

    func testAnUnreachableReactionListingEndsTheRunNonZeroWithNoReport() async throws {
        let (result, pi) = try await failingRun(.reactionListingUnreachable)
        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertTrue(pi.reactionRequests.isEmpty)
        XCTAssertTrue(result.output.stdoutText.isEmpty)
    }

    func testARejectedSetReactionSubmissionEndsTheRunNonZeroWithNoReport() async throws {
        let (result, pi) = try await failingRun(.reactionSubmission(1))
        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertNil(pi.facts(of: "love"))
        XCTAssertTrue(result.output.stdoutText.isEmpty)
    }

    func testAnUnreachableSetReactionSubmissionEndsTheRunNonZeroWithNoReport() async throws {
        let (result, pi) = try await failingRun(.reactionSubmissionUnreachable)
        XCTAssertNotEqual(result.exitCode, 0)
        XCTAssertNil(pi.facts(of: "love"))
        XCTAssertTrue(result.output.stdoutText.isEmpty)
    }

    private func fileTree(_ root: URL) throws -> [String] {
        let enumerator = FileManager.default.enumerator(atPath: root.path)
        return (enumerator?.allObjects as? [String] ?? []).sorted()
    }
}
