import XCTest
@testable import CRMMacPiClient

final class RequestBuilderTests: XCTestCase {
    private let baseURL = URL(string: "https://pi.example.test")!
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "key")

    func testPairOmitsAuthHeaders() throws {
        let req = try RequestBuilder(baseURL: baseURL).pair(
            token: "abc", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)
        XCTAssertEqual(req.url?.absoluteString, "https://pi.example.test/api/v1/host")
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Content-Type"), "application/json")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Accept"), "application/json")
        XCTAssertNil(req.value(forHTTPHeaderField: "X-Mac-Host-ID"))
        XCTAssertNil(req.value(forHTTPHeaderField: "Authorization"))
        XCTAssertNotNil(req.httpBody)
    }

    func testHeartbeatHasAuthHeaders() throws {
        let body = HeartbeatBody(
            daemonVersion: "0.1.0",
            protocolVersion: 1,
            permissions: Data("{}".utf8),
            sourceHealth: Data("{}".utf8))
        let req = try RequestBuilder(baseURL: baseURL).heartbeat(auth: auth, body: body)
        XCTAssertEqual(
            req.url?.absoluteString,
            "https://pi.example.test/api/v1/host/\(auth.hostID.uuidString.lowercased())/heartbeat")
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Mac-Host-ID")?.lowercased(), auth.hostID.uuidString.lowercased())
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer key")
    }

    func testKnownIdentifiersIsGET() throws {
        let req = try RequestBuilder(baseURL: baseURL).knownIdentifiers(auth: auth)
        XCTAssertEqual(req.httpMethod, "GET")
        XCTAssertNil(req.httpBody)
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Mac-Host-ID")?.lowercased(), auth.hostID.uuidString.lowercased())
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer key")
    }

    func testBaseURLWithTrailingSlashCollapses() throws {
        let req = try RequestBuilder(baseURL: URL(string: "https://pi.example.test/")!).pair(
            token: "abc", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)
        XCTAssertEqual(req.url?.absoluteString, "https://pi.example.test/api/v1/host")
    }

    func testHeartbeatBodyEncodesSnakeCaseAndPermissions() throws {
        let body = HeartbeatBody(
            daemonVersion: "0.1.0",
            protocolVersion: 2,
            permissions: Data("{\"fda\": true}".utf8),
            sourceHealth: Data("{}".utf8))
        let req = try RequestBuilder(baseURL: baseURL).heartbeat(auth: auth, body: body)
        let json = try JSONSerialization.jsonObject(with: req.httpBody!) as! [String: Any]
        XCTAssertEqual(json["daemon_version"] as? String, "0.1.0")
        XCTAssertEqual(json["protocol_version"] as? Int, 2)
        let permissions = json["permissions"] as? [String: Any]
        XCTAssertEqual(permissions?["fda"] as? Bool, true)
    }

    // MARK: - needsAttention

    func testNeedsAttentionPathAndQuery() throws {
        let req = try RequestBuilder(baseURL: baseURL)
            .needsAttention(auth: auth, hostID: auth.hostID)
        XCTAssertEqual(req.httpMethod, "GET")
        XCTAssertNil(req.httpBody)
        XCTAssertEqual(req.url?.path, "/api/v1/meeting-notes/needs-attention")
        let comps = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)
        let queryItems = comps?.queryItems ?? []
        XCTAssertEqual(queryItems.count, 1)
        XCTAssertEqual(queryItems[0].name, "host_id")
        XCTAssertEqual(queryItems[0].value, auth.hostID.uuidString.lowercased())
    }

    func testNeedsAttentionSendsHostAuthHeadersNotApiKey() throws {
        // Regression test pinning the host-auth header shape. The
        // daemon must NEVER send X-API-Key for this endpoint — it
        // doesn't possess the global API key. A future refactor
        // adding X-API-Key would silently 401 in production because
        // the daemon's pair key isn't the global key.
        let req = try RequestBuilder(baseURL: baseURL)
            .needsAttention(auth: auth, hostID: auth.hostID)
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Mac-Host-ID")?.lowercased(),
                       auth.hostID.uuidString.lowercased())
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"),
                       "Bearer \(auth.apiKey)")
        XCTAssertNil(req.value(forHTTPHeaderField: "X-API-Key"))
    }

    func testNeedsAttentionLowercasesHostIDInQuery() throws {
        // Even if a caller (e.g. test that constructs a UUID via a
        // mixed-case string) passes a non-lowercase UUID, the
        // builder lowercases it to match the Pi's canonical form.
        let mixedCase = UUID(uuidString: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")!
        let req = try RequestBuilder(baseURL: baseURL)
            .needsAttention(auth: auth, hostID: mixedCase)
        let comps = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)
        XCTAssertEqual(comps?.queryItems?.first?.value,
                       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
    }

    func testNeedsAttentionHonorsBaseURLTrailingSlash() throws {
        let req = try RequestBuilder(baseURL: URL(string: "https://pi.example.test/")!)
            .needsAttention(auth: auth, hostID: auth.hostID)
        XCTAssertEqual(req.url?.path, "/api/v1/meeting-notes/needs-attention")
    }
}
