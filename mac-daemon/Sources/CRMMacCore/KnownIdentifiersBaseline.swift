// KnownIdentifiersBaseline — the persisted shape of one source's
// known-identifiers baseline.
//
// On daemon restart the composition root reads each source's baseline
// from DaemonState.knownIdentifierBaselines and seeds the in-memory
// KnownIdentifiersCache diff baseline. The heartbeat refresher is the
// SOLE writer of the persisted baseline; it advances the baseline to
// the observed known set MINUS any identifier still owed a durable scan
// enqueue (see KnownIdentifiersCache.persistableBaseline).
//
// Storing the canonical handle list lets the daemon compute a PRECISE
// `current − persisted` delta after an offline restart and enqueue a
// 30-day backwards scan ONLY for genuine additions — never the naive
// "set changed → re-scan everything" replay storm.
//
// PII-at-rest footprint: the canonical handle list is the minimal
// representation the cache already holds in memory; it lives in the
// same protected state.json as the rest of the daemon state.
import Foundation

public struct KnownIdentifiersBaseline: Codable, Equatable, Sendable {
    /// Sorted canonical handle list (E.164 phones / lowercased emails).
    public var canonical: [String]

    /// When this source's baseline was first established. Observability
    /// only — not consumed by the diff logic. Preserved across refresher
    /// writes so it records the first-seed time, not the last update.
    public var establishedAt: Date

    public init(canonical: [String], establishedAt: Date) {
        self.canonical = canonical
        self.establishedAt = establishedAt
    }

    enum CodingKeys: String, CodingKey {
        case canonical
        case establishedAt = "established_at"
    }

    /// Materialize the canonical list as an unordered Set for diffing.
    public func toSet() -> Set<String> {
        Set(canonical)
    }

    /// Build a baseline from an unordered set, sorting the canonical
    /// list so the persisted JSON is deterministic across runs.
    public static func from(set: Set<String>, establishedAt: Date) -> KnownIdentifiersBaseline {
        KnownIdentifiersBaseline(canonical: set.sorted(), establishedAt: establishedAt)
    }
}
