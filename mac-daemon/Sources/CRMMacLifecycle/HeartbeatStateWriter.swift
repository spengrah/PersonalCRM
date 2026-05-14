// On-disk implementation of HeartbeatStateWriter (the protocol lives
// in HeartbeatLoop.swift). Routes the state mutation through the
// shared `StateMutator` actor so concurrent writers (heartbeat loop +
// messages source plugin + future source plugins) serialize.
//
// Atomic-rename in StateStore.save() is NOT sufficient by itself —
// it prevents torn writes (a partial JSON file) but does NOT prevent
// stale overwrite when two writers race on load -> mutate -> save.
// The actor's mutate() closes that window.
import Foundation
import CRMMacCore

public final class OnDiskHeartbeatStateWriter: HeartbeatStateWriter {
    private let mutator: StateMutator
    private let logger: LoggerProtocol

    public init(mutator: StateMutator, logger: LoggerProtocol) {
        self.mutator = mutator
        self.logger = logger
    }

    public func recordSuccessfulHeartbeat(at: Date, cursorEpoch: Int64) async throws {
        do {
            try await mutator.mutate { state in
                state.lastHeartbeatAt = at
                // Heartbeat does not write cursorEpoch into state.json —
                // cursors are per-source state owned by SourcePlugins.
                // The parameter is part of the protocol so source
                // readers can observe the Pi's current epoch (e.g. for
                // backup-restore detection); persistence of that signal
                // is a per-source concern.
                _ = cursorEpoch
            }
        } catch {
            logger.warning("heartbeat: state persist failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw error
        }
    }
}
