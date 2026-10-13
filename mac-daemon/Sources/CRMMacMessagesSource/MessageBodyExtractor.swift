// MessageBodyExtractor — the one place a chat.db row's body is decided.
//
// macOS stores a message body in `message.text`, in `message.attributedBody`
// (an NSAttributedString archived as a typedstream), or in both. Newer
// macOS versions often leave `text` NULL and carry the body only in
// `attributedBody`. Every path that shapes a published message from
// chat.db (live tick, backfill, operator scan) reads its body through
// `extract`, so no path can disagree about what a row says.
import Foundation

public enum MessageBodyResult: Equatable, Sendable {
    /// The plain-text body when `text` has body text, else the decoded
    /// rich-text body.
    case body(String)
    /// Neither column holds body text. Attachment placeholder characters
    /// (U+FFFC) and whitespace do not count as body text.
    case absent
    /// `attributedBody` is present, `text` holds no body text, and no body
    /// can be recovered from `attributedBody`.
    case undecodable
}

public enum MessageBodyExtractor {
    public static func extract(text: String?, attributedBody: Data?) -> MessageBodyResult {
        if let text, hasBodyText(text) { return .body(text) }
        guard let attributedBody else { return .absent }
        guard let decoded = decodeTypedstreamString(attributedBody) else { return .undecodable }
        return hasBodyText(decoded) ? .body(decoded) : .absent
    }

    private static let attachmentPlaceholder: Character = "\u{FFFC}"

    private static func hasBodyText(_ s: String) -> Bool {
        s.contains { $0 != attachmentPlaceholder && !$0.isWhitespace }
    }

    /// Reads the string out of an archived NSAttributedString. The typedstream
    /// holds the class chain (`... NSString`), then `01 .. .. 01 2B` (the `+`
    /// type tag for a C string), a typedstream integer giving the byte
    /// length, and the UTF-8 bytes. Integers below 0x80 are the value itself;
    /// 0x81 and 0x82 prefix a 2-byte and a 4-byte little-endian value.
    private static func decodeTypedstreamString(_ data: Data) -> String? {
        let bytes = [UInt8](data)
        let classMarker = [UInt8]("NSString".utf8)
        guard let markerEnd = firstIndex(of: classMarker, in: bytes, from: 0).map({ $0 + classMarker.count }),
              let tag = firstIndex(of: [0x01, 0x2B], in: bytes, from: markerEnd) else {
            return nil
        }
        var i = tag + 2
        guard i < bytes.count else { return nil }
        let length: Int
        switch bytes[i] {
        case 0x81:
            guard i + 2 < bytes.count else { return nil }
            length = Int(bytes[i + 1]) | Int(bytes[i + 2]) << 8
            i += 3
        case 0x82:
            guard i + 4 < bytes.count else { return nil }
            length = (1...4).reduce(0) { $0 | Int(bytes[i + $1]) << (8 * ($1 - 1)) }
            i += 5
        case let b where b < 0x80:
            length = Int(b)
            i += 1
        default:
            return nil
        }
        guard length <= bytes.count - i else { return nil }
        return String(bytes: bytes[i..<(i + length)], encoding: .utf8)
    }

    private static func firstIndex(of needle: [UInt8], in haystack: [UInt8], from start: Int) -> Int? {
        guard haystack.count >= needle.count, start <= haystack.count - needle.count else { return nil }
        for i in start...(haystack.count - needle.count)
        where haystack[i..<(i + needle.count)].elementsEqual(needle) {
            return i
        }
        return nil
    }
}
