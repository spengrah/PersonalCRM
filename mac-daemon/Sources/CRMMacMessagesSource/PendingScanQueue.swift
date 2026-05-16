// PendingScanQueue — small wrapper around an array of PendingScan
// entries, with a hard cap.
//
// The queue's authoritative location is the Pi-side opaque cursor
// (MessagesCursor.pendingScans).  This in-memory wrapper is used by
// the source plugin to dequeue-and-process during a tick; on commit-
// cursor success the plugin writes the remaining queue back into the
// cursor JSON so a crash between dequeue and publish recovers (next
// tick re-emits via event-log dedup).
import Foundation

public struct PendingScanQueue: Equatable, Sendable {
    public private(set) var entries: [PendingScan]

    public init(_ entries: [PendingScan] = []) {
        // Apply cap on construction (defensive — the cursor JSON also
        // applies the cap when writing).
        self.entries = Self.enforceCap(entries)
    }

    public var isEmpty: Bool {
        entries.isEmpty
    }

    public var count: Int {
        entries.count
    }

    /// Append a scan request. If the queue is at cap, drops the OLDEST
    /// entry. Returns true if an entry was dropped (caller logs a
    /// warning).
    @discardableResult
    public mutating func enqueue(_ scan: PendingScan) -> Bool {
        entries.append(scan)
        if entries.count > MessagesCursor.pendingScansCap {
            entries.removeFirst()
            return true
        }
        return false
    }

    /// Remove and return the front of the queue. Returns nil if empty.
    public mutating func dequeue() -> PendingScan? {
        guard !entries.isEmpty else { return nil }
        return entries.removeFirst()
    }

    /// Replace the queue contents.
    public mutating func replace(with entries: [PendingScan]) {
        self.entries = Self.enforceCap(entries)
    }

    private static func enforceCap(_ entries: [PendingScan]) -> [PendingScan] {
        guard entries.count > MessagesCursor.pendingScansCap else { return entries }
        return Array(entries.suffix(MessagesCursor.pendingScansCap))
    }
}
