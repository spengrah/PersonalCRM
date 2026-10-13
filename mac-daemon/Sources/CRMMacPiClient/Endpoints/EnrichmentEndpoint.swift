// EnrichmentEndpoint — wire shapes for the message-enrichment exchange:
//   GET  /api/v1/host/:id/messages/enrichment/missing-bodies?after=<guid>&limit=<1..500>
//   POST /api/v1/host/:id/messages/enrichment
//
// Both answer the standard {success, data, ...} envelope; this file declares
// the `data` payloads and the request body. The one op the exchange carries
// today is `fill_body`: it sets the body of a stored row that has none and
// changes nothing else.
import Foundation

/// One listed body-less stored row. The guid is all the daemon needs to
/// look the body up in the messages database.
public struct MissingBodyItem: Decodable, Equatable, Sendable {
    public let guid: String

    public init(guid: String) {
        self.guid = guid
    }
}

/// Decoded `data` payload of the missing-bodies listing. `nextAfter` is the
/// last listed guid when more rows remain, else nil.
public struct MissingBodiesPage: Decodable, Equatable, Sendable {
    public let items: [MissingBodyItem]
    public let nextAfter: String?

    public init(items: [MissingBodyItem], nextAfter: String?) {
        self.items = items
        self.nextAfter = nextAfter
    }

    private enum CodingKeys: String, CodingKey {
        case items
        case nextAfter = "next_after"
    }
}

/// One `fill_body` request item.
public struct FillBodyItem: Encodable, Equatable, Sendable {
    public let guid: String
    public let body: String

    public init(guid: String, body: String) {
        self.guid = guid
        self.body = body
    }

    private enum CodingKeys: String, CodingKey {
        case op, guid, body
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode("fill_body", forKey: .op)
        try c.encode(guid, forKey: .guid)
        try c.encode(body, forKey: .body)
    }
}

/// What the Pi did with one `fill_body` item.
public enum FillBodyOutcome: String, Decodable, Equatable, Sendable {
    /// The row had no body and now has the submitted one.
    case filled
    /// The row already had a body; it keeps it.
    case unchanged
    /// The Pi holds no live row with the guid; nothing was created.
    case unknown
}

public struct FillBodyResult: Decodable, Equatable, Sendable {
    public let guid: String
    public let outcome: FillBodyOutcome

    public init(guid: String, outcome: FillBodyOutcome) {
        self.guid = guid
        self.outcome = outcome
    }
}

/// Decoded `data` payload of the enrichment POST: one result per request
/// item, in request order. The Pi's `totals` are not decoded; the results
/// carry the same information per guid.
public struct FillBodiesData: Decodable, Equatable, Sendable {
    public let results: [FillBodyResult]

    public init(results: [FillBodyResult]) {
        self.results = results
    }
}

struct FillBodiesRequestBody: Encodable {
    let items: [FillBodyItem]
}

extension RequestBuilder {
    func missingBodies(auth: PiAuth, after: String?, limit: Int) throws -> URLRequest {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let after { query.insert(URLQueryItem(name: "after", value: after), at: 0) }
        let url = try enrichmentURL(
            auth: auth, suffix: "/messages/enrichment/missing-bodies", query: query)
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func fillBodies(auth: PiAuth, items: [FillBodyItem]) throws -> URLRequest {
        let url = try enrichmentURL(auth: auth, suffix: "/messages/enrichment", query: [])
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        do {
            req.httpBody = try JSONEncoder().encode(FillBodiesRequestBody(items: items))
        } catch {
            throw RequestBuilderError.encode("encode enrichment body: \(error)")
        }
        return req
    }

    private func enrichmentURL(
        auth: PiAuth, suffix: String, query: [URLQueryItem]
    ) throws -> URL {
        guard var comps = URLComponents(url: baseURL, resolvingAgainstBaseURL: false) else {
            throw RequestBuilderError.malformedURL(baseURL.absoluteString)
        }
        let basePath = comps.path.hasSuffix("/")
            ? String(comps.path.dropLast())
            : comps.path
        comps.path = basePath + "/api/v1/host/\(auth.hostID.uuidString.lowercased())" + suffix
        if !query.isEmpty { comps.queryItems = query }
        guard let url = comps.url else {
            throw RequestBuilderError.malformedURL(baseURL.absoluteString)
        }
        return url
    }
}
