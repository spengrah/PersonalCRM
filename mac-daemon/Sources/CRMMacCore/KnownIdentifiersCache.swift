// KnownIdentifiersCache — in-memory canonicalized handle set + per-
// source diff.
//
// The cache stores normalized phone+email identifiers received from
// the Pi via GET /api/v1/host/:id/known-identifiers. It serves two
// purposes:
//
//   1. Sender filter — every chat.db / CallHistoryDB row goes through
//      `contains` before payload shaping; non-members are dropped. This
//      is an OPTIMIZATION (the Pi's ingest still accepts events with no
//      matching contact_method, staging the row with NULL contact_id);
//      the daemon-side filter just reduces noise.
//
//   2. Newly-known-contact detection — every heartbeat tick refreshes
//      the cache; each source's `drainNewlyAdded(for:)` reports
//      identifiers that appeared since that source last drained so the
//      source plugin can enqueue a 30-day backwards scan for each.
//
// Per-source design. The cache is SHARED across both messages and
// phone_calls (one instance, constructed at the composition root). Each
// source has its own DIFF baseline + newly-added bucket so one source
// draining a newly-added identifier does NOT empty the other's view.
//
// Restart / persistence semantics. The in-memory state is rebuilt each
// process start. To enqueue 30-day scans for ONLY the identifiers added
// while the daemon was offline (never the whole set), each source's
// diff baseline is seeded at construction from the persisted
// DaemonState.knownIdentifierBaselines. The heartbeat refresher is the
// SOLE writer of the persisted baseline; it reads `persistableBaseline`
// (the observed set minus identifiers still owed a durable scan) and
// plain-assigns it. The source tick NEVER writes the persisted
// baseline — it only clears an identifier's "owed" status via the
// drain → commit → confirmDrained flow so the next refresher persist
// can include it.
//
// Baseline tri-state (per source). A source's diff baseline is either
// ABSENT (key missing → `noBaseline` → seed on first fetch, enqueue
// nothing) or PRESENT (a Set, possibly empty → diff precisely). A
// present-but-empty baseline is a REAL empty baseline (an empty CRM),
// NOT the same as absent: a later identifier appearing offline diffs
// against `∅` and IS scanned.
import Foundation
import CryptoKit

public actor KnownIdentifiersCache {
    /// The current known set (drives `contains` sender filter).
    private var canonicalSet: Set<String> = []
    /// True once the FIRST `replace(with:)` has run, regardless of
    /// whether the fetched set was empty. Distinguishes "no fetch yet"
    /// from "fetched an empty set" for the Phase-B scan-readiness gate.
    private var fetched: Bool = false
    /// The fixed set of consumers (source ids) passed at construction.
    private let consumers: Set<SourceID>
    /// Each consumer's DIFF baseline. Key ABSENT = `noBaseline` (seed
    /// on first fetch, no scans). Key PRESENT (possibly empty Set) =
    /// real baseline (diff precisely). Trails the latest fetch after
    /// the first fetch for an established consumer.
    private var baseline: [SourceID: Set<String>] = [:]
    /// Per-consumer newly-added bucket — identifiers observed-as-new but
    /// not yet drained by that consumer's source tick.
    private var pendingNewlyAdded: [SourceID: Set<String>] = [:]
    /// Per-consumer scan-queue-drain holding set. `drainNewlyAdded`
    /// MOVES the bucket here; `confirmDrained` clears it after the
    /// Phase-A cursor commit; `returnInFlight` rolls it back on commit
    /// failure. Together `pendingNewlyAdded[S] ∪ inFlight[S]` = S's
    /// "owed scans", which `persistableBaseline` subtracts.
    private var inFlight: [SourceID: Set<String>] = [:]

    /// Construct the cache. `consumers` + `baselines` are fixed at
    /// construction (the async composition root). Plugins do NOT
    /// self-register. A source absent from `baselines` starts
    /// `noBaseline` (seed-on-first-fetch, upgrade boundary).
    public init(
        initial: Set<String> = [],
        baselines: [SourceID: Set<String>] = [:],
        consumers: Set<SourceID> = []
    ) {
        self.canonicalSet = initial
        self.consumers = consumers
        self.baseline = baselines
    }

    /// True when no fetch has populated the cache yet. The row-emitting
    /// batches skip when this is false (sender filter would drop
    /// everything; at-most-one wasted tick on cold start).
    public var isPopulated: Bool {
        !canonicalSet.isEmpty
    }

    /// True once the first `replace(with:)` has run (even of an empty
    /// set). Gates Phase-B scan execution: a not-yet-fetched cache must
    /// NOT adjudicate (drop) pending scans, but an empty-but-fetched
    /// CRM must.
    public var hasFetched: Bool {
        fetched
    }

    /// O(1) membership check.
    public func contains(_ canonical: String) -> Bool {
        canonicalSet.contains(canonical)
    }

    /// Snapshot of the current canonical set (defensive copy).
    public func snapshot() -> Set<String> {
        canonicalSet
    }

    /// Replace the cache contents and update each consumer's diff
    /// baseline + newly-added bucket.
    ///
    /// For each consumer S:
    ///   - If `baseline[S]` is ABSENT (`noBaseline`): seed
    ///     `baseline[S] = fetched` (even if empty) and enqueue NOTHING.
    ///     The upgrade boundary / first fetch never replays the set.
    ///   - Else (present, possibly empty): `added = fetched − baseline[S]
    ///     − inFlight[S]` (excluding in-flight so a drained-but-not-yet-
    ///     confirmed identifier isn't re-added to the bucket); append
    ///     `added` to S's bucket; then `baseline[S] = fetched` (the diff
    ///     baseline trails the latest fetch — additions AND removals
    ///     fold in for the NEXT diff).
    public func replace(with fetchedSet: Set<String>) {
        for consumer in consumers {
            if baseline[consumer] == nil {
                // noBaseline → seed, enqueue nothing.
                baseline[consumer] = fetchedSet
            } else {
                let added = fetchedSet
                    .subtracting(baseline[consumer] ?? [])
                    .subtracting(inFlight[consumer] ?? [])
                if !added.isEmpty {
                    pendingNewlyAdded[consumer, default: []].formUnion(added)
                }
                baseline[consumer] = fetchedSet
            }
        }
        canonicalSet = fetchedSet
        fetched = true
    }

    /// Drain consumer `id`'s newly-added bucket NON-DESTRUCTIVELY:
    /// MOVES it into `inFlight[id]` and returns it. The source tick
    /// enqueues a scan per returned identifier and commits the cursor;
    /// on success it calls `confirmDrained(for:)`, on failure
    /// `returnInFlight(for:)`. Empty if the consumer was just seeded
    /// (`noBaseline`) this cycle.
    public func drainNewlyAdded(for id: SourceID) -> Set<String> {
        let drained = pendingNewlyAdded[id] ?? []
        pendingNewlyAdded[id] = []
        if !drained.isEmpty {
            inFlight[id, default: []].formUnion(drained)
        }
        return drained
    }

    /// Clear consumer `id`'s in-flight holding set after its Phase-A
    /// cursor commit succeeds. The drained identifiers are now durably
    /// enqueued, so they leave the "owed" set and become eligible for
    /// the next refresher persist. NO baseline argument, NO baseline
    /// side effect.
    public func confirmDrained(for id: SourceID) {
        let confirmed = inFlight[id] ?? []
        inFlight[id] = []
        // Defensive: an identifier re-detected as new between drain and
        // confirm could be sitting in pendingNewlyAdded; subtract the
        // confirmed set so it isn't re-scanned redundantly. (The
        // refresher's `replace` already excludes inFlight, so this is
        // belt-and-suspenders.)
        if !confirmed.isEmpty, let bucket = pendingNewlyAdded[id], !bucket.isEmpty {
            pendingNewlyAdded[id] = bucket.subtracting(confirmed)
        }
    }

    /// Roll consumer `id`'s in-flight set back into its newly-added
    /// bucket after a Phase-A cursor-commit failure, so the next tick
    /// re-drains. Idempotent.
    public func returnInFlight(for id: SourceID) {
        let held = inFlight[id] ?? []
        inFlight[id] = []
        if !held.isEmpty {
            pendingNewlyAdded[id, default: []].formUnion(held)
        }
    }

    /// The in-memory DIFF baseline for consumer `id` (nil if
    /// `noBaseline`). Drives `added` in `replace`. Used by tests; the
    /// refresher persists the narrower `persistableBaseline`.
    public func baseline(for id: SourceID) -> Set<String>? {
        baseline[id]
    }

    /// The WRITABLE baseline for consumer `id`: `baseline[id] −
    /// pendingNewlyAdded[id] − inFlight[id]` (nil if `noBaseline`) — the
    /// observed set MINUS identifiers still owed a durable scan enqueue.
    /// The refresher (sole persisted-baseline writer) reads this as an
    /// immutable Sendable value OUTSIDE its synchronous mutate closure,
    /// then plain-assigns it. Excluding owed scans guarantees the
    /// persisted baseline never advances past an identifier whose 30-day
    /// scan isn't durable yet, so a crash before the scan is committed
    /// leaves the restart delta able to re-detect and re-enqueue it.
    public func persistableBaseline(for id: SourceID) -> Set<String>? {
        guard let base = baseline[id] else { return nil }
        return base
            .subtracting(pendingNewlyAdded[id] ?? [])
            .subtracting(inFlight[id] ?? [])
    }
}

/// SHA-256 hash of the sorted canonical set, hex-encoded lowercase.
///
/// Retained for the dead `knownIdentifiersHash` cursor field (its
/// removal is out of scope). Sorting before hashing makes the hash
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
