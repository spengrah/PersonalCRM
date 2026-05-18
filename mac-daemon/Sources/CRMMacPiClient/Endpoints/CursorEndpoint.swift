// CursorEndpoint — wire shapes for GET/POST /api/v1/host/:id/sync/:source/cursor.
//
// Mirrors backend/internal/api/handlers/mac_host.go:260-396 exactly.
//
// GET returns {cursor, cursor_epoch, backfill_complete} wrapped in the
// standard api.APIResponse envelope. The daemon unwraps via
// APIEnvelope<MessagesCursorState>.
//
// POST takes {cursor, base_cursor, cursor_epoch, backfill_complete} as
// the request body. Success: data: {"ok": true}. Conflict (HTTP 409):
//   {success: false,
//    error: {code: "EPOCH_MISMATCH" | "BASE_CURSOR_MISMATCH", message},
//    data: {current_cursor, current_epoch}}
import Foundation

/// Decoded body of GET cursor endpoint. Source-agnostic — every
/// per-source plugin reads this shape from
/// `/api/v1/host/:id/sync/:source/cursor`. The legacy name
/// `MessagesCursorState` lives on as a typealias so existing call
/// sites keep compiling; new code should use `SourceCursorState`.
public struct SourceCursorState: Decodable, Equatable, Sendable {
    public let cursor: String
    public let cursorEpoch: Int64
    public let backfillComplete: Bool

    public init(cursor: String, cursorEpoch: Int64, backfillComplete: Bool) {
        self.cursor = cursor
        self.cursorEpoch = cursorEpoch
        self.backfillComplete = backfillComplete
    }

    enum CodingKeys: String, CodingKey {
        case cursor
        case cursorEpoch       = "cursor_epoch"
        case backfillComplete  = "backfill_complete"
    }
}

/// Legacy alias — the type used to be named for the messages source
/// when it was the only consumer. New code references
/// `SourceCursorState` directly.
public typealias MessagesCursorState = SourceCursorState

/// POST cursor request body. Encoded into the request payload.
struct CommitCursorBody: Encodable, Equatable {
    let cursor: String
    let baseCursor: String
    let cursorEpoch: Int64
    let backfillComplete: Bool

    enum CodingKeys: String, CodingKey {
        case cursor
        case baseCursor       = "base_cursor"
        case cursorEpoch      = "cursor_epoch"
        case backfillComplete = "backfill_complete"
    }
}

/// 409 conflict body. Top-level `data` field of the APIResponse.
public struct CursorConflict: Decodable, Equatable, Sendable {
    public let currentCursor: String?
    public let currentEpoch: Int64?

    public init(currentCursor: String?, currentEpoch: Int64?) {
        self.currentCursor = currentCursor
        self.currentEpoch = currentEpoch
    }

    enum CodingKeys: String, CodingKey {
        case currentCursor = "current_cursor"
        case currentEpoch  = "current_epoch"
    }
}

/// Codes the Pi returns on 409 cursor-commit conflict.
public enum ConflictCode: String, Decodable, Sendable, Equatable {
    case epochMismatch      = "EPOCH_MISMATCH"
    case baseCursorMismatch = "BASE_CURSOR_MISMATCH"
}
