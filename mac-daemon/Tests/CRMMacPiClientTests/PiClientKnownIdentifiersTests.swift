import XCTest
@testable import CRMMacPiClient

final class PiClientKnownIdentifiersTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func client(_ script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    func testKnownIdentifiers200() async throws {
        let data = try loadFixture("known_identifiers_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        let result = try await client(script).knownIdentifiers(auth: auth)
        XCTAssertEqual(result.phones.count, 2)
        XCTAssertEqual(result.emails.count, 2)
    }

    func testKnownIdentifiers401Propagates() async throws {
        let data = try loadFixture("heartbeat_401")
        let script = MockTransportScript([.respond(status: 401, data: data)])
        await assertThrows(client(script).knownIdentifiers(auth: auth)) { error in
            guard case PiClientError.authenticationRevoked = error else {
                XCTFail("got \(error)")
                return
            }
        }
    }

    func testKnownIdentifiersEmptyArrays() async throws {
        let data = Data("""
        {"success": true, "data": {"phones": [], "emails": []}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 200, data: data)])
        let result = try await client(script).knownIdentifiers(auth: auth)
        XCTAssertTrue(result.phones.isEmpty)
        XCTAssertTrue(result.emails.isEmpty)
    }

    func testKnownIdentifiersGETShape() async throws {
        let data = try loadFixture("known_identifiers_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        _ = try await client(script).knownIdentifiers(auth: auth)
        XCTAssertEqual(script.invocations[0].httpMethod, "GET")
        XCTAssertNil(script.invocations[0].httpBody, "GET should have no body")
        XCTAssertEqual(
            script.invocations[0].url?.path,
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/known-identifiers")
    }
}
