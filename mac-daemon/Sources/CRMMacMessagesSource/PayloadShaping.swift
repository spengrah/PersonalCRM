// PayloadShaping — shapes ChatDBMessage rows into the Pi-side
// RawMessage*Payload JSON envelope.
//
// Wire keys mirror backend/internal/events/kinds.go:308 exactly.
// snake_case via explicit CodingKeys; the daemon does NOT use a
// `keyEncodingStrategy = .convertToSnakeCase` shortcut so the wire
// shape is documented at the type level.
//
// Outbound peer (is_from_me=1, usually NULL handle_id): the caller
// resolves via ChatDBReader.outboundPeer (the first chat_handle_join
// entry by join ROWID) before invoking shape. For group chats this is
// a v1 simplification — every outbound group message attributes to the
// same peer. There is no self-handle exclusion; it relies on macOS
// excluding the account owner's handle from chat_handle_join.
import Foundation

/// One attachment's metadata, per the Pi-side AttachmentMeta. Currently
/// unused (Attachments stays empty in v1 per spec §3 / brief
/// resolution), but defined here so the wire-shape contract is
/// expressed at the type level for the test goldens.
public struct AttachmentMeta: Encodable, Equatable, Sendable {
    public let type: MessageType
    public let filename: String?
    public let mimeType: String?
    public let size: Int64?

    enum CodingKeys: String, CodingKey {
        case type
        case filename
        case mimeType = "mime_type"
        case size
    }

    public init(type: MessageType, filename: String? = nil,
                mimeType: String? = nil, size: Int64? = nil) {
        self.type = type
        self.filename = filename
        self.mimeType = mimeType
        self.size = size
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(type.rawValue, forKey: .type)
        try c.encodeIfPresent(filename, forKey: .filename)
        try c.encodeIfPresent(mimeType, forKey: .mimeType)
        try c.encodeIfPresent(size, forKey: .size)
    }
}

/// The wire envelope sent as the `payload` of an IngestEvent. Same
/// struct shape for `raw_message.received` and `raw_message.sent` —
/// the kind discriminator on the IngestEvent envelope (NOT the
/// payload) tells the Pi which direction.
public struct RawMessagePayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    public let guid: String
    public let chatID: String
    public let peerHandle: String
    public let peerName: String?
    public let text: String?
    public let messageType: MessageType
    public let isGroup: Bool
    public let sentAt: Date
    public let replyToGuid: String?
    public let attachments: [AttachmentMeta]

    enum CodingKeys: String, CodingKey {
        case version
        case hostID      = "host_id"
        case source
        case guid
        case chatID      = "chat_id"
        case peerHandle  = "peer_handle"
        case peerName    = "peer_name"
        case text
        case messageType = "message_type"
        case isGroup     = "is_group"
        case sentAt      = "sent_at"
        case replyToGuid = "reply_to_guid"
        case attachments
    }

    public init(
        version: Int = 1,
        hostID: UUID,
        source: String = "messages",
        guid: String,
        chatID: String,
        peerHandle: String,
        peerName: String? = nil,
        text: String? = nil,
        messageType: MessageType,
        isGroup: Bool,
        sentAt: Date,
        replyToGuid: String? = nil,
        attachments: [AttachmentMeta] = []
    ) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.guid = guid
        self.chatID = chatID
        self.peerHandle = peerHandle
        self.peerName = peerName
        self.text = text
        self.messageType = messageType
        self.isGroup = isGroup
        self.sentAt = sentAt
        self.replyToGuid = replyToGuid
        self.attachments = attachments
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        // Encode UUID as lowercase string for Go wire-shape parity:
        // Swift's default UUID encoding emits uppercase hex, but Go's
        // uuid.UUID.MarshalJSON emits lowercase. The Pi accepts both
        // forms but the byte-for-byte parity fixture requires lowercase.
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(guid, forKey: .guid)
        try c.encode(chatID, forKey: .chatID)
        try c.encode(peerHandle, forKey: .peerHandle)
        try c.encodeIfPresent(peerName, forKey: .peerName)
        try c.encodeIfPresent(text, forKey: .text)
        try c.encode(messageType.rawValue, forKey: .messageType)
        try c.encode(isGroup, forKey: .isGroup)
        try c.encode(sentAt, forKey: .sentAt)
        try c.encodeIfPresent(replyToGuid, forKey: .replyToGuid)
        // Attachments is omitempty on the Pi side; skip if empty for
        // wire-shape parity.
        if !attachments.isEmpty {
            try c.encode(attachments, forKey: .attachments)
        }
    }
}

/// Direction discriminator. The IngestEvent envelope's `kind` carries
/// this; the payload struct is identical.
public enum MessageDirection: String, Sendable {
    case received = "raw_message.received"
    case sent     = "raw_message.sent"
}

public enum PayloadShaping {
    /// Convert a ChatDBMessage row into a (kind, payload) pair, given
    /// the peer handle and host UUID. The peer is supplied by the
    /// caller because outbound rows usually carry a NULL message.handle_id
    /// and need an explicit outboundPeer lookup (the chat's
    /// chat_handle_join entry) that the page fetch doesn't do.
    public static func shape(
        row: ChatDBMessage,
        peerHandle: String,
        hostID: UUID
    ) -> (kind: MessageDirection, payload: RawMessagePayload) {
        let kind: MessageDirection = row.isFromMe ? .sent : .received

        let utis: [String]
        if let uti = row.primaryAttachmentUTI, !uti.isEmpty {
            utis = [uti]
        } else {
            utis = []
        }
        let hasText = (row.text?.isEmpty == false)
        let messageType = UTIMapping.resolve(attachmentUTIs: utis, hasText: hasText)

        let payload = RawMessagePayload(
            version: CRMMacMessagesSource.payloadVersion,
            hostID: hostID,
            source: "messages",
            guid: row.guid,
            chatID: row.chatGUID ?? "",
            peerHandle: peerHandle,
            peerName: nil, // v1: chat.db display names left out per the spec
            text: row.text,
            messageType: messageType,
            isGroup: row.isGroup,
            sentAt: row.sentAt,
            replyToGuid: row.replyToGUID,
            attachments: []) // v1: spec keeps Attachments empty
        return (kind, payload)
    }
}
