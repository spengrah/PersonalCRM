// PiClientKnownIDsTests cover the new GET
// /api/v1/host/:id/sync/:source/known-ids endpoint the icloud_contacts
// source plugin's recovery flow uses to drive tombstone
// reconciliation against the live CNContactStore scan.
import XCTest
import Foundation
@testable import CRMMacPiClient

final class PiClientKnownIDsTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func client(_ script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    func testKnownIDs200() async throws {
        let data = try loadFixture("known_ids_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        let result = try await client(script).knownIDs(auth: auth, source: "icloud_contacts")
        XCTAssertEqual(result.ids.count, 3)
        XCTAssertEqual(result.ids[0].sourceID, "contact-A@abc123")
        XCTAssertEqual(result.ids[0].lastContentHash, "abc123")
        XCTAssertEqual(result.ids[1].sourceID, "contact-B@def456")
        XCTAssertEqual(result.ids[1].lastContentHash, "def456")
        XCTAssertEqual(result.ids[2].sourceID, "contact-legacy@h")
        XCTAssertNil(result.ids[2].lastContentHash,
                     "legacy rows decode with nil last_content_hash")
    }

    func testKnownIDs200Empty() async throws {
        let data = try loadFixture("known_ids_200_empty")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        let result = try await client(script).knownIDs(auth: auth, source: "icloud_contacts")
        XCTAssertTrue(result.ids.isEmpty)
    }

    func testKnownIDs401Propagates() async throws {
        let data = try loadFixture("heartbeat_401")
        let script = MockTransportScript([.respond(status: 401, data: data)])
        await assertThrows({
            try await self.client(script).knownIDs(auth: self.auth, source: "icloud_contacts")
        }) { error in
            guard case PiClientError.authenticationRevoked = error else {
                XCTFail("got \(error)")
                return
            }
        }
    }

    func testKnownIDsGenericClientError() async throws {
        let data = Data("""
        {"success": false, "error": {"code": "FORBIDDEN", "message": "no"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 403, data: data)])
        await assertThrows({
            try await self.client(script).knownIDs(auth: self.auth, source: "icloud_contacts")
        }) { error in
            guard case PiClientError.clientError(let status, _, _) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(status, 403)
        }
    }

    func testKnownIDsServerError5xx() async throws {
        let script = MockTransportScript(
            Array(repeating: .respond(status: 500, data: Data("{}".utf8)), count: 6))
        await assertThrows({
            try await self.client(script).knownIDs(auth: self.auth, source: "icloud_contacts")
        }) { error in
            // The retrying transport surfaces 5xx as PiClientError.serverError
            // after exhausting retries.
            guard case PiClientError.serverError(let status, _) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(status, 500)
        }
    }

    func testKnownIDsGETShape() async throws {
        let data = try loadFixture("known_ids_200_empty")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        _ = try await client(script).knownIDs(auth: auth, source: "icloud_contacts")
        XCTAssertEqual(script.invocations[0].httpMethod, "GET")
        XCTAssertNil(script.invocations[0].httpBody, "GET should have no body")
        XCTAssertEqual(
            script.invocations[0].url?.path,
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/sync/icloud_contacts/known-ids")
    }
}
