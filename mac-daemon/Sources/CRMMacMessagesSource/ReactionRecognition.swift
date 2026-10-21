// ReactionRecognition — the one step that decides whether a chat.db message
// row is a reaction and which facts it carries. Every daemon read of the
// messages database (the live tick, the backfill, the scan, the group
// re-scan's chat window, and the re-scan script's body and reaction passes)
// decides through `facts`; no read path keeps its own list of reaction types.
//
// chat.db `message.associated_message_type`:
//   2000...2007  add a reaction: love, like, dislike, laugh, emphasize,
//                question, emoji, sticker
//   3000...3007  remove the reaction of the same kind
//   1000         a sticker placed on a message, the older sticker-reaction
//                form, when the row has a target; it has no removal type
// `message.associated_message_guid` names the target with a part prefix
// (`p:<n>/`) or a `bp:` prefix, or bare. `message.associated_message_emoji`
// holds the emoji of an emoji reaction and of its removal.
import Foundation
import CRMMacPiClient

public enum ReactionRecognition {
    /// The reaction facts of a row, or nil when the row is not a reaction
    /// this step recognizes: a type outside the ranges above, no target, or
    /// an emoji reaction without its emoji.
    public static func facts(
        associatedMessageType type: Int64,
        associatedMessageGUID: String?,
        associatedMessageEmoji emoji: String?
    ) -> MessageReaction? {
        guard let rawTarget = associatedMessageGUID else { return nil }
        let targetKey = stripPrefix(rawTarget)
        if type == 1000 {
            return MessageReaction(kind: .sticker, targetKey: targetKey, removal: false)
        }
        let removal: Bool
        let offset: Int64
        switch type {
        case 2000...2007: (removal, offset) = (false, type - 2000)
        case 3000...3007: (removal, offset) = (true, type - 3000)
        default: return nil
        }
        let kind: MessageReaction.Kind
        switch offset {
        case 0: kind = .love
        case 1: kind = .like
        case 2: kind = .dislike
        case 3: kind = .laugh
        case 4: kind = .emphasize
        case 5: kind = .question
        case 6:
            guard let emoji else { return nil }
            kind = .emoji(emoji)
        case 7: kind = .sticker
        default: return nil
        }
        return MessageReaction(kind: kind, targetKey: targetKey, removal: removal)
    }

    /// `raw` without a leading `bp:` or `p:<digits>/`, so it equals the
    /// target message's guid.
    static func stripPrefix(_ raw: String) -> String {
        if raw.hasPrefix("bp:") { return String(raw.dropFirst(3)) }
        if raw.hasPrefix("p:"), let slash = raw.firstIndex(of: "/") {
            let part = raw[raw.index(raw.startIndex, offsetBy: 2)..<slash]
            if !part.isEmpty, part.allSatisfy(\.isASCIIDigit) {
                return String(raw[raw.index(after: slash)...])
            }
        }
        return raw
    }
}

private extension Character {
    var isASCIIDigit: Bool { ("0"..."9").contains(self) }
}
