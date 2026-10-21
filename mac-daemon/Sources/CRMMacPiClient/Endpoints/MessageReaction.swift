// MessageReaction — the `reaction` object a message carries when it is a
// reaction to another message. One wire shape serves both places it travels:
// the raw message payload of the ingest exchange and the `set_reaction` item
// of the enrichment exchange.
//
//   { "kind": "love | like | dislike | laugh | emphasize | question | emoji | sticker",
//     "emoji": "<present exactly when kind is emoji>",
//     "target_key": "<the target message's guid>",
//     "removal": <bool> }
//
// The Pi stores an invalid object on a live message as no reaction, and
// rejects a `set_reaction` request holding one. The type makes an invalid
// object unconstructable: the emoji rides on the emoji kind alone, and the
// initializer refuses an empty target key or an empty emoji.
import Foundation

public struct MessageReaction: Encodable, Equatable, Sendable {
    public enum Kind: Equatable, Sendable {
        case love, like, dislike, laugh, emphasize, question
        case emoji(String)
        case sticker
    }

    public let kind: Kind
    /// The guid the target message is sent and stored under.
    public let targetKey: String
    /// True when the message takes back an earlier reaction of the same kind.
    public let removal: Bool

    public init?(kind: Kind, targetKey: String, removal: Bool) {
        if targetKey.isEmpty { return nil }
        if case .emoji(let emoji) = kind, emoji.isEmpty { return nil }
        self.kind = kind
        self.targetKey = targetKey
        self.removal = removal
    }

    enum CodingKeys: String, CodingKey {
        case kind, emoji, removal
        case targetKey = "target_key"
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        let name: String
        switch kind {
        case .love: name = "love"
        case .like: name = "like"
        case .dislike: name = "dislike"
        case .laugh: name = "laugh"
        case .emphasize: name = "emphasize"
        case .question: name = "question"
        case .emoji(let emoji):
            name = "emoji"
            try c.encode(emoji, forKey: .emoji)
        case .sticker: name = "sticker"
        }
        try c.encode(name, forKey: .kind)
        try c.encode(targetKey, forKey: .targetKey)
        try c.encode(removal, forKey: .removal)
    }
}
