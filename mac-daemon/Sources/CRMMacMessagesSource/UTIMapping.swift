// UTIMapping — maps `attachment.uti` (Uniform Type Identifier strings
// from chat.db) to the Pi-side `MessageType` bucket.
//
// The Pi accepts these buckets in the raw_message payload's
// `message_type` field (events/kinds.go).  V1 simplification: we read
// ONE primary attachment per message (the first by
// message_attachment_join.ROWID) and pick its bucket. Multi-attachment
// messages take the first non-`other` bucket per the spec resolution.
//
// Pure value-type logic; no system frameworks.
import Foundation

/// Bucketed message-type discriminator. Matches the Pi-side values.
public enum MessageType: String, Equatable, Sendable {
    case text
    case photo
    case audio
    case video
    case document
    case other
}

public enum UTIMapping {
    /// Map a single UTI string to its bucket. Returns `.other` for
    /// unknown UTIs.
    public static func bucket(forUTI uti: String) -> MessageType {
        let lower = uti.lowercased()

        // Image
        if lower == "public.image"
            || lower.hasPrefix("public.image.")
            || lower == "public.png"
            || lower == "public.jpeg"
            || lower == "public.heic"
            || lower == "public.heif"
            || lower == "com.compuserve.gif"
            || lower == "com.microsoft.bmp" {
            return .photo
        }

        // Audio
        if lower == "public.audio"
            || lower == "public.mp3"
            || lower == "public.aifc-audio"
            || lower == "public.aiff-audio"
            || lower == "com.apple.coreaudio-format"
            || lower == "com.apple.m4a-audio"
            || lower == "org.xiph.flac"
            || lower == "org.xiph.ogg-vorbis" {
            return .audio
        }

        // Video
        if lower == "public.movie"
            || lower == "public.mpeg-4"
            || lower == "public.mpeg"
            || lower == "com.apple.quicktime-movie"
            || lower == "com.apple.m4v-video"
            || lower == "public.avi" {
            return .video
        }

        // Document
        if lower == "com.adobe.pdf"
            || lower == "public.pdf"
            || lower == "com.microsoft.word.doc"
            || lower == "org.openxmlformats.wordprocessingml.document"
            || lower == "public.plain-text"
            || lower == "public.text"
            || lower == "public.composite-content"
            || lower == "public.rtf"
            || lower == "com.apple.iwork.pages.pages" {
            return .document
        }

        return .other
    }

    /// Pick the bucket from a list of attachment UTIs. First non-`other`
    /// wins; falls back to `.other` if every attachment is unknown.
    /// Empty list -> `.other` (caller decides whether `.text` is correct
    /// based on whether the message has text content).
    public static func bucket(forAttachmentUTIs utis: [String]) -> MessageType {
        for uti in utis {
            let candidate = bucket(forUTI: uti)
            if candidate != .other {
                return candidate
            }
        }
        return .other
    }

    /// Resolve the final message_type given the attachments and whether
    /// the message has text content.
    ///   - no attachments + text     -> .text
    ///   - no attachments + no text  -> .other (rare; sticker-only?)
    ///   - any attachments           -> bucket(forAttachmentUTIs:) result
    ///                                  (or .other if all attachments
    ///                                  are .other)
    public static func resolve(attachmentUTIs: [String], hasText: Bool) -> MessageType {
        if attachmentUTIs.isEmpty {
            return hasText ? .text : .other
        }
        return bucket(forAttachmentUTIs: attachmentUTIs)
    }
}
