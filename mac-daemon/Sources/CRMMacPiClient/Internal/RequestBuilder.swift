// RequestBuilder produces URLRequests for the Pi endpoints. The pair
// request omits auth headers; every other request injects
// `X-Mac-Host-ID` + `Authorization: Bearer <key>` to satisfy
// MacHostAuthMiddleware on the Pi side.
import Foundation
import CRMMacCore

/// Encapsulates the per-host auth pair. The pair request omits this
/// entirely; everything else requires it.
public struct PiAuth: Equatable, Sendable {
    public let hostID: UUID
    public let apiKey: String

    public init(hostID: UUID, apiKey: String) {
        self.hostID = hostID
        self.apiKey = apiKey
    }
}

/// Pre-formatted pair request body.
struct PairRequestBody: Encodable {
    let pairingToken: String
    let hostname: String
    let daemonVersion: String
    let protocolVersion: Int32

    private enum CodingKeys: String, CodingKey {
        case pairingToken = "pairing_token"
        case hostname
        case daemonVersion = "daemon_version"
        case protocolVersion = "protocol_version"
    }
}

/// Pre-formatted rotate-key request body.
struct RotateAPIKeyRequestBody: Encodable {
    let pairingToken: String

    private enum CodingKeys: String, CodingKey {
        case pairingToken = "pairing_token"
    }
}

enum RequestBuilderError: Error {
    case malformedURL(String)
    case encode(String)
}

struct RequestBuilder {
    let baseURL: URL

    init(baseURL: URL) {
        self.baseURL = baseURL
    }

    func pair(
        token: String,
        hostname: String,
        daemonVersion: String,
        protocolVersion: Int32
    ) throws -> URLRequest {
        let url = try resolve(path: "/api/v1/host")
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        let body = PairRequestBody(
            pairingToken: token,
            hostname: hostname,
            daemonVersion: daemonVersion,
            protocolVersion: protocolVersion)
        do {
            req.httpBody = try JSONEncoder().encode(body)
        } catch {
            throw RequestBuilderError.encode("encode pair body: \(error)")
        }
        return req
    }

    func rotateAPIKey(auth: PiAuth, pairingToken: String) throws -> URLRequest {
        let url = try resolve(path:
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/rotate-key")
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        let body = RotateAPIKeyRequestBody(pairingToken: pairingToken)
        do {
            req.httpBody = try JSONEncoder().encode(body)
        } catch {
            throw RequestBuilderError.encode("encode rotate-key body: \(error)")
        }
        return req
    }

    func heartbeat(auth: PiAuth, body: HeartbeatBody) throws -> URLRequest {
        let url = try resolve(path: "/api/v1/host/\(auth.hostID.uuidString.lowercased())/heartbeat")
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        do {
            req.httpBody = try JSONEncoder().encode(body)
        } catch {
            throw RequestBuilderError.encode("encode heartbeat body: \(error)")
        }
        return req
    }

    func knownIdentifiers(auth: PiAuth) throws -> URLRequest {
        let url = try resolve(path: "/api/v1/host/\(auth.hostID.uuidString.lowercased())/known-identifiers")
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func getCursor(auth: PiAuth, source: String) throws -> URLRequest {
        let url = try resolve(path:
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/sync/\(source)/cursor")
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func commitCursor(
        auth: PiAuth,
        source: String,
        cursor: String,
        baseCursor: String,
        cursorEpoch: Int64,
        backfillComplete: Bool
    ) throws -> URLRequest {
        let url = try resolve(path:
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/sync/\(source)/cursor")
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        let body = CommitCursorBody(
            cursor: cursor,
            baseCursor: baseCursor,
            cursorEpoch: cursorEpoch,
            backfillComplete: backfillComplete)
        do {
            req.httpBody = try JSONEncoder().encode(body)
        } catch {
            throw RequestBuilderError.encode("encode commit cursor body: \(error)")
        }
        return req
    }

    func knownIDs(auth: PiAuth, source: String) throws -> URLRequest {
        let url = try resolve(path:
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/sync/\(source)/known-ids")
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func needsAttention(auth: PiAuth, hostID: UUID) throws -> URLRequest {
        // Query param is percent-encoded by URLComponents. The host
        // ID is also lowercased to match the Pi's canonical form.
        guard var comps = URLComponents(url: baseURL, resolvingAgainstBaseURL: false) else {
            throw RequestBuilderError.malformedURL(baseURL.absoluteString)
        }
        let basePath = comps.path.hasSuffix("/")
            ? String(comps.path.dropLast())
            : comps.path
        comps.path = basePath + "/api/v1/meeting-notes/needs-attention"
        comps.queryItems = [
            URLQueryItem(name: "host_id", value: hostID.uuidString.lowercased()),
        ]
        guard let url = comps.url else {
            throw RequestBuilderError.malformedURL(baseURL.absoluteString)
        }
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        // Reuses the existing per-host auth helper. Sends
        // X-Mac-Host-ID + Authorization: Bearer <pair-key>. The Pi's
        // /meeting-notes/needs-attention route is mounted under the
        // composite IngestAuth middleware (same dispatcher that
        // accepts the daemon's host-auth on /ingest/events), so this
        // path resolves without needing the global env-var API key.
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func ingestEvents(auth: PiAuth, body: IngestEventsBody) throws -> URLRequest {
        let url = try resolve(path: "/api/v1/ingest/events")
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.withoutEscapingSlashes]
        do {
            req.httpBody = try encoder.encode(body)
        } catch {
            throw RequestBuilderError.encode("encode ingest body: \(error)")
        }
        return req
    }

    private func resolve(path: String) throws -> URL {
        guard var comps = URLComponents(url: baseURL, resolvingAgainstBaseURL: false) else {
            throw RequestBuilderError.malformedURL(baseURL.absoluteString)
        }
        // Trim any trailing slash on the base path so the join is
        // deterministic regardless of how the operator supplied the URL.
        let basePath = comps.path.hasSuffix("/")
            ? String(comps.path.dropLast())
            : comps.path
        comps.path = basePath + path
        guard let url = comps.url else {
            throw RequestBuilderError.malformedURL(baseURL.absoluteString)
        }
        return url
    }

    static func applyAuth(_ req: inout URLRequest, auth: PiAuth) {
        req.setValue(auth.hostID.uuidString.lowercased(), forHTTPHeaderField: "X-Mac-Host-ID")
        req.setValue("Bearer \(auth.apiKey)", forHTTPHeaderField: "Authorization")
    }
}
