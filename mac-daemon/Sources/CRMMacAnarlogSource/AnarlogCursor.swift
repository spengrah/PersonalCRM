// AnarlogCursor — cursor codecs for the two anarlog reader plugins.
//
// The cursor is the literal map shape spec line 498 documents
// (`{uuid: <per-entry-shape>}`) — an intentional extension of the
// spec to carry the payload_hash needed for deterministic
// `<uuid>@deleted@<hash>` source_ids without a Pi round-trip on
// every delete.
//
// Spec extensions / owned deviations:
//   - humans: spec is `{content_hash, mtime}`; we add `payload_hash`.
//   - sessions: spec is `{meta_mtime, meta_hash, summary_hash,
//     memo_hash}`. We drop `meta_mtime` (mtime never drives skip
//     decisions in this implementation) and add `payload_hash`.
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
    /// SHA-256 of the file bytes — per spec line 185. Drives change
    /// detection.
    public let contentHash: String
    /// SHA-256 of the encoded wire payload — drives the source_id for
    /// future deletes. Distinct concept from `contentHash`.
    public let payloadHash: String
    /// Modification time at scan time, in epoch milliseconds.
    /// Diagnostic only; NOT used for skip decisions.
    public let mtimeEpochMs: Int64?

    public init(contentHash: String, payloadHash: String, mtimeEpochMs: Int64? = nil) {
        self.contentHash = contentHash
        self.payloadHash = payloadHash
        self.mtimeEpochMs = mtimeEpochMs
    }

    enum CodingKeys: String, CodingKey {
        case contentHash    = "content_hash"
        case payloadHash    = "payload_hash"
        case mtimeEpochMs   = "mtime_epoch_ms"
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
    /// SHA-256 of `_meta.json` bytes per spec line 196. The literal
    /// `"floor_skip"` sentinel marks pre-backfill-floor sessions —
    /// those never emit an event.
    public let metaHash: String
    /// SHA-256 of `_summary.md` bytes; nil when the file is absent.
    public let summaryHash: String?
    /// SHA-256 of `_memo.md` bytes; nil when the file is absent.
    public let memoHash: String?
    /// SHA-256 of the encoded wire payload — for future delete
    /// source_ids. Empty string on floor_skip sentinels (they never
    /// emit a payload).
    public let payloadHash: String

    public init(
        metaHash: String,
        summaryHash: String? = nil,
        memoHash: String? = nil,
        payloadHash: String
    ) {
        self.metaHash = metaHash
        self.summaryHash = summaryHash
        self.memoHash = memoHash
        self.payloadHash = payloadHash
    }

    /// True when this entry is the pre-floor sentinel — used by the
    /// tombstone branch to skip emitting deletes for sessions that
    /// were never published in the first place.
    public var isFloorSkipped: Bool {
        metaHash == AnarlogSessionsCursorCodec.floorSkipMarker
    }

    enum CodingKeys: String, CodingKey {
        case metaHash    = "meta_hash"
        case summaryHash = "summary_hash"
        case memoHash    = "memo_hash"
        case payloadHash = "payload_hash"
    }
}

public enum AnarlogSessionsCursorCodec {
    /// Sentinel value placed in `metaHash` to mark pre-floor sessions
    /// for sessions older than the backfill floor. These cursor
    /// entries exist so the same session isn't
    /// re-evaluated every tick, but never produce events.
    public static let floorSkipMarker = "floor_skip"

    /// Construct a pre-floor sentinel entry.
    public static func floorSkippedEntry() -> AnarlogSessionsCursorEntry {
        AnarlogSessionsCursorEntry(
            metaHash: floorSkipMarker,
            summaryHash: nil,
            memoHash: nil,
            payloadHash: "")
    }

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

// MARK: - Tombstone basis

/// Prior cursor entry used by the tombstone diff. The basis is either
/// the prior cursor map (delta route) or `/known-ids` results
/// (bootstrap / recovery routes). Both surfaces carry a UUID + an
/// optional prior payload hash; we reduce both to this shape so the
/// tombstone-emission loop is route-agnostic.
public struct AnarlogTombstoneBasisEntry: Equatable, Sendable {
    public let uuid: String
    /// Prior payload hash used to construct
    /// `<uuid>@deleted@<priorPayloadHash>`. Nil falls back to
    /// `@deleted@unknown`.
    public let priorPayloadHash: String?
    /// True when this entry is the pre-floor sentinel — used by the
    /// tombstone branch to skip emitting deletes for sessions that
    /// were never published in the first place.
    public let isFloorSkipped: Bool

    public init(
        uuid: String,
        priorPayloadHash: String?,
        isFloorSkipped: Bool = false
    ) {
        self.uuid = uuid
        self.priorPayloadHash = priorPayloadHash
        self.isFloorSkipped = isFloorSkipped
    }
}
