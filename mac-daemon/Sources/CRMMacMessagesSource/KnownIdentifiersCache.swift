// KnownIdentifiersCache — in-memory canonicalized handle set + diff.
//
// The cache stores normalized phone+email identifiers received from
// the Pi via GET /api/v1/host/:id/known-identifiers. It serves two
// purposes:
//
//   1. Sender filter — every chat.db row goes through `contains` before
//      payload shaping; non-members are dropped. This is an OPTIMIZATION
//      (per spec §2 + plan §"Sender filter integration"), not a contract:
//      the Pi's handleRawMessage still accepts events with no matching
//      contact_method, staging the row with NULL contact_id. Daemon-side
//      filter just reduces noise.
//
//   2. Newly-known-contact detection — every heartbeat tick refreshes
//      the cache; `diff` reports identifiers that appeared since the
//      last fetch so the messages plugin can enqueue a 30-day backwards
//      scan for each.
//
// Restart semantics (plan §R9): the cache is in-memory; on daemon
// restart it starts empty and is repopulated by the first heartbeat
// fetch. To avoid replaying the full set as "new" on every restart, the
// daemon persists the SHA-256 hash of the sorted canonical set inside
// the cursor JSON. On restart, the freshly-fetched set's hash is
// compared to the persisted one; if equal -> normal operation, if
// different -> log info, do NOT auto-queue scans for every member.
// The operator runs `crm-mac messages scan --identifier <X>` manually
// for any contacts added during downtime.
import Foundation
import CryptoKit

public actor KnownIdentifiersCache {
    private var canonicalSet: Set<String> = []

    public init(initial: Set<String> = []) {
        self.canonicalSet = initial
    }

    /// True when no fetch has populated the cache yet. The messages tick
    /// must skip emitting work in that case (sender filter would drop
    /// everything; we want at-most-one wasted tick on cold start).
    public var isPopulated: Bool {
        !canonicalSet.isEmpty
    }

    /// O(1) membership check.
    public func contains(_ canonical: String) -> Bool {
        canonicalSet.contains(canonical)
    }

    /// Snapshot of the current canonical set (defensive copy).
    public func snapshot() -> Set<String> {
        canonicalSet
    }

    /// Replace the cache contents and return the set of new
    /// identifiers (newly-fetched - previously-cached). The previous
    /// contents are dropped. Identifiers removed from the new fetch
    /// (Pi-side deletions) are NOT returned — diff is one-way (only
    /// additions), matching the spec's "trigger 30-day scan on new
    /// contact" semantics.
    public func replace(with fetched: Set<String>) -> Set<String> {
        let added = fetched.subtracting(canonicalSet)
        canonicalSet = fetched
        return added
    }
}

/// SHA-256 hash of the sorted canonical set, hex-encoded lowercase.
///
/// Used in MessagesCursor.knownIdentifiersHash for restart-time change
/// detection (plan §R9).  Sorting before hashing makes the hash
/// deterministic regardless of insertion order.
public enum KnownIdentifiersHash {
    public static func sha256Hex(of set: Set<String>) -> String {
        let sorted = set.sorted()
        // Use NUL separator (\0) so handles containing newlines/commas
        // don't collide.
        let canonical = sorted.joined(separator: "\0")
        let digest = SHA256.hash(data: Data(canonical.utf8))
        return digest.map { String(format: "%02x", $0) }.joined()
    }
}
