// EnrichmentEndpoint — wire shapes for the message-enrichment exchange:
//   GET  /api/v1/host/:id/messages/enrichment/missing-bodies?after=<guid>&limit=<1..500>
//   GET  /api/v1/host/:id/messages/enrichment/group-chats?after=<chat_guid>&limit=<1..500>
//   GET  /api/v1/host/:id/messages/enrichment/without-reaction-facts?after=<guid>&limit=<1..500>
//   POST /api/v1/host/:id/messages/enrichment
//
// All answer the standard {success, data, ...} envelope; this file declares
// the `data` payloads and the request body. The exchange carries four ops:
// `fill_body` sets the body of a stored row that has none; `add_group_message`
// adds a group message the Pi lacks inside a window of a stored group chat;
// `clear_outbound_peer` removes the member attribution of a stored outbound
// group row; `set_reaction` gives a stored row that carries no reaction facts
// its facts. None of them replays any ingest effect.
import Foundation

/// One listed body-less stored row. The guid is all the daemon needs to
/// look the body up in the messages database.
public struct MissingBodyItem: Decodable, Equatable, Sendable {
    public let guid: String

    public init(guid: String) {
        self.guid = guid
    }
}

/// Decoded `data` payload of the missing-bodies listing, and of the
/// without-reaction-facts listing, which answers in the same page shape.
/// `nextAfter` is the last listed guid when more rows remain, else nil.
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

/// One listed group chat and the windows of its messages interactions.
public struct GroupChatItem: Decodable, Equatable, Sendable {
    public let chatGUID: String
    public let windows: [GroupChatWindow]

    public init(chatGUID: String, windows: [GroupChatWindow]) {
        self.chatGUID = chatGUID
        self.windows = windows
    }

    private enum CodingKeys: String, CodingKey {
        case chatGUID = "chat_guid"
        case windows
    }
}

/// The closed span of sent times of one interaction's messages.
public struct GroupChatWindow: Decodable, Equatable, Sendable {
    public let start: Date
    public let end: Date

    public init(start: Date, end: Date) {
        self.start = start
        self.end = end
    }

    private enum CodingKeys: String, CodingKey {
        case start, end
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        start = try Self.decodeTime(c, .start)
        end = try Self.decodeTime(c, .end)
    }

    /// RFC 3339, with or without fractional seconds.
    private static func decodeTime(
        _ c: KeyedDecodingContainer<CodingKeys>, _ key: CodingKeys
    ) throws -> Date {
        let text = try c.decode(String.self, forKey: key)
        let plain = ISO8601DateFormatter()
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        guard let date = plain.date(from: text) ?? fractional.date(from: text) else {
            throw DecodingError.dataCorruptedError(
                forKey: key, in: c, debugDescription: "not an RFC 3339 time")
        }
        return date
    }
}

/// Decoded `data` payload of the group-chats listing. `nextAfter` is the last
/// listed chat guid when more chats remain, else nil.
public struct GroupChatsPage: Decodable, Equatable, Sendable {
    public let items: [GroupChatItem]
    public let nextAfter: String?

    public init(items: [GroupChatItem], nextAfter: String?) {
        self.items = items
        self.nextAfter = nextAfter
    }

    private enum CodingKeys: String, CodingKey {
        case items
        case nextAfter = "next_after"
    }
}

/// One enrichment request item of any op. A request may mix them.
public enum EnrichmentItem: Encodable, Equatable, Sendable {
    case addGroupMessage(AddGroupMessageItem)
    case clearOutboundPeer(guid: String)
    case setReaction(guid: String, reaction: MessageReaction)

    public func encode(to encoder: Encoder) throws {
        switch self {
        case .addGroupMessage(let item):
            try item.encode(to: encoder)
        case .clearOutboundPeer(let guid):
            var c = encoder.container(keyedBy: AddGroupMessageItem.CodingKeys.self)
            try c.encode("clear_outbound_peer", forKey: .op)
            try c.encode(guid, forKey: .guid)
        case .setReaction(let guid, let reaction):
            var c = encoder.container(keyedBy: SetReactionKeys.self)
            try c.encode("set_reaction", forKey: .op)
            try c.encode(guid, forKey: .guid)
            try c.encode(reaction, forKey: .reaction)
        }
    }

    private enum SetReactionKeys: String, CodingKey {
        case op, guid, reaction
    }
}

/// One `add_group_message` request item. `sender` is the member's raw handle
/// for an inbound message and nil for the user's own; `body` is nil when the
/// message has none. Both encode as an explicit null.
public struct AddGroupMessageItem: Encodable, Equatable, Sendable {
    public enum Direction: String, Encodable, Sendable {
        case inbound, outbound
    }

    public let guid: String
    public let chatGUID: String
    public let sender: String?
    public let direction: Direction
    public let sentAt: Date
    public let body: String?

    public init(
        guid: String, chatGUID: String, sender: String?,
        direction: Direction, sentAt: Date, body: String?
    ) {
        self.guid = guid
        self.chatGUID = chatGUID
        self.sender = sender
        self.direction = direction
        self.sentAt = sentAt
        self.body = body
    }

    enum CodingKeys: String, CodingKey {
        case op, guid, sender, direction, body
        case chatGUID = "chat_guid"
        case sentAt = "sent_at"
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode("add_group_message", forKey: .op)
        try c.encode(guid, forKey: .guid)
        try c.encode(chatGUID, forKey: .chatGUID)
        try c.encode(sender, forKey: .sender)
        try c.encode(direction, forKey: .direction)
        try c.encode(sentAt, forKey: .sentAt)
        try c.encode(body, forKey: .body)
    }
}

/// What the Pi did with one item of the enrichment exchange.
public enum EnrichmentOutcome: String, Decodable, Equatable, Sendable, CaseIterable {
    case filled, unchanged, unknown, added
    case outsideWindow = "outside_window"
    case cleared
    /// A `set_reaction` stored its facts on a row that carried none.
    case marked
}

public struct EnrichmentResult: Decodable, Equatable, Sendable {
    public let guid: String
    public let outcome: EnrichmentOutcome

    public init(guid: String, outcome: EnrichmentOutcome) {
        self.guid = guid
        self.outcome = outcome
    }
}

/// Decoded `data` payload of an enrichment POST carrying any ops: one result
/// per request item, in request order. `totals` is not decoded; the results
/// carry the same information.
public struct EnrichmentData: Decodable, Equatable, Sendable {
    public let results: [EnrichmentResult]

    public init(results: [EnrichmentResult]) {
        self.results = results
    }
}

struct EnrichmentRequestBody: Encodable {
    let items: [EnrichmentItem]
}

public enum EnrichmentWire {
    /// The encoder every enrichment request body is written with. Times use
    /// `.iso8601` (whole seconds), as the messages publisher encodes `sent_at`.
    public static func makeEncoder() -> JSONEncoder {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }
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

    func groupChats(auth: PiAuth, after: String?, limit: Int) throws -> URLRequest {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let after { query.insert(URLQueryItem(name: "after", value: after), at: 0) }
        let url = try enrichmentURL(
            auth: auth, suffix: "/messages/enrichment/group-chats", query: query)
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func withoutReactionFacts(auth: PiAuth, after: String?, limit: Int) throws -> URLRequest {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let after { query.insert(URLQueryItem(name: "after", value: after), at: 0) }
        let url = try enrichmentURL(
            auth: auth, suffix: "/messages/enrichment/without-reaction-facts", query: query)
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        return req
    }

    func fillBodies(auth: PiAuth, items: [FillBodyItem]) throws -> URLRequest {
        try enrichmentPost(auth: auth, body: FillBodiesRequestBody(items: items))
    }

    func enrichment(auth: PiAuth, items: [EnrichmentItem]) throws -> URLRequest {
        try enrichmentPost(auth: auth, body: EnrichmentRequestBody(items: items))
    }

    private func enrichmentPost<Body: Encodable>(auth: PiAuth, body: Body) throws -> URLRequest {
        let url = try enrichmentURL(auth: auth, suffix: "/messages/enrichment", query: [])
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        Self.applyAuth(&req, auth: auth)
        do {
            req.httpBody = try EnrichmentWire.makeEncoder().encode(body)
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
