// CRMMacMessagesSource — the chat.db reader + MessagesSourcePlugin.
//
// Target layering: depends on CRMMacCore, CRMMacPiClient, and GRDB.swift.
// Isolated from other targets so GRDB is not transitively pulled into
// Foundation-only modules (CRMMacCore, CRMMacLifecycle).
//
// Source files:
//   - UTIMapping.swift              attachment.uti -> MessageType bucketing
//   - HandleNormalization.swift     thin re-export of CRMMacCore normalizers
//   - MessagesCursor.swift          JSON-packed Pi-side opaque cursor
//   - KnownIdentifiersCache.swift   in-memory canonical-handle set + diff
//   - PendingScanQueue.swift        queued targeted scans
//   - ChatDBSchema.swift            whitelist + schema-drift detection
//   - ChatDBReader.swift            GRDB-backed row iterator
//   - BackfillPacing.swift          (count, wallclock) budget tracker
//   - PayloadShaping.swift          ChatDBMessage -> RawMessage*Payload
//   - MessagesPublisher.swift       /ingest/events batching
//   - MessagesSourcePlugin.swift    SourcePlugin actor + tick orchestration
//   - PayloadSource.swift           protocol seam for plugin tests
//
// This placeholder file is a versioned anchor so the target compiles
// before any production source lands. It exposes a single empty enum
// (no instantiation, no semantics) used only by the trivial build-shell
// test in CRMMacMessagesSourceTests.
import Foundation

public enum CRMMacMessagesSource {
    /// Versioned anchor — bumped when the source ships breaking changes
    /// to the Pi-side payload contract.
    public static let payloadVersion: Int = 1
}
