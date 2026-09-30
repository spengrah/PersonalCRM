// AnarlogCursor — cursor codecs for the two anarlog reader plugins.
//
// The cursor is the literal map shape spec line 498 documents
// (`{uuid: <per-entry-shape>}`) — an intentional extension of the
// spec to carry the payload_hash needed for deterministic
// `<uuid>@deleted@<hash>` source_ids without a Pi round-trip on
// every delete.
//
// Spec extensions / owned deviations:
//   - humans: an entry is `{record_hash}`, the payload hash of the person's last shaped upsert.
//   - sessions: `record_hash` is the content hash of the encoded
//     meeting_note.recorded payload last shaped from the CLI record.
//
// The cursor IS the literal `{uuid → entry}` map at the JSON root.
// No `{version, ...}` wrapper. Future schema bumps will be handled
// via the cursor-reset path, not in-place versioning.
//
// Decoding: `decodeOrNil` returns nil on empty string OR malformed
// JSON OR per-entry decode failure. The nil return is what routes
// the tick into the bootstrap-via-known-ids path. Returning
// an empty `[:]` would be wrong — it would signal "I have a cursor;
// it's just empty" and route to the .delta path, which would emit
// tombstones for everything the Pi has on file.
import Foundation

// MARK: - Humans

public struct AnarlogHumansCursorEntry: Codable, Equatable, Sendable {
    public let recordHash: String

    public init(recordHash: String) {
        self.recordHash = recordHash
    }

    enum CodingKeys: String, CodingKey {
        case recordHash = "record_hash"
    }
}

public enum AnarlogHumansCursorCodec {
    /// Encode a `{uuid → entry}` map to a JSON string. Keys are
    /// sorted so the byte output is stable across re-encodes (matters
    /// for the cursor commit's base-cursor compare on the Pi).
    public static func encode(_ map: [String: AnarlogHumansCursorEntry]) throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(map)
        return String(data: data, encoding: .utf8) ?? ""
    }

    /// Decode a cursor string into the `{uuid → entry}` map.
    /// Returns nil for empty string, malformed JSON, or any per-entry
    /// decode failure (strict). The nil return is what routes the
    /// tick into bootstrap-via-known-ids.
    public static func decodeOrNil(_ s: String) -> [String: AnarlogHumansCursorEntry]? {
        if s.isEmpty { return nil }
        guard let data = s.data(using: .utf8) else { return nil }
        let decoder = JSONDecoder()
        return try? decoder.decode([String: AnarlogHumansCursorEntry].self, from: data)
    }
}

// MARK: - Sessions

public struct AnarlogSessionsCursorEntry: Codable, Equatable, Sendable {
    /// Content hash of the encoded meeting_note.recorded payload last
    /// shaped from this CLI record. It drives both re-sends and the
    /// prior hash in a deletion source ID.
    public let recordHash: String

    public init(recordHash: String) {
        self.recordHash = recordHash
    }

    enum CodingKeys: String, CodingKey {
        case recordHash = "record_hash"
    }
}

public enum AnarlogSessionsCursorCodec {
    public static func encode(_ map: [String: AnarlogSessionsCursorEntry]) throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(map)
        return String(data: data, encoding: .utf8) ?? ""
    }

    public static func decodeOrNil(_ s: String) -> [String: AnarlogSessionsCursorEntry]? {
        if s.isEmpty { return nil }
        guard let data = s.data(using: .utf8) else { return nil }
        let decoder = JSONDecoder()
        return try? decoder.decode([String: AnarlogSessionsCursorEntry].self, from: data)
    }
}
