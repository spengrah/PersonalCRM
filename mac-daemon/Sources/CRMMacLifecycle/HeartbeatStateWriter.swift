// On-disk implementation of HeartbeatStateWriter (the protocol lives
// in HeartbeatLoop.swift). Performs a load -> mutate -> save against
// StateStore so the daemon's last-heartbeat timestamp survives a
// restart.
//
// Concurrency note: load -> mutate -> save is NOT atomic across
// concurrent writers, only the save() itself is. Today the heartbeat
// loop is the only writer of state.json so this is safe; once source
// plugins begin committing per-source cursors, state writes need to
// be serialized — see PluginRegistry / DaemonRunner for the expected
// actor barrier.
import Foundation
import CRMMacCore

public final class OnDiskHeartbeatStateWriter: HeartbeatStateWriter {
    private let stateStore: StateStore
    private let logger: LoggerProtocol

    public init(stateStore: StateStore, logger: LoggerProtocol) {
        self.stateStore = stateStore
        self.logger = logger
    }

    public func recordSuccessfulHeartbeat(at: Date, cursorEpoch: Int64) {
        do {
            var state = try stateStore.load()
            state.lastHeartbeatAt = at
            // Heartbeat does not write cursorEpoch into state.json —
            // cursors are per-source state owned by SourcePlugins. The
            // parameter is part of the protocol so source readers can
            // observe the Pi's current epoch (e.g. for backup-restore
            // detection); persistence of that signal is a per-source
            // concern.
            _ = cursorEpoch
            try stateStore.save(state)
        } catch {
            logger.warning("heartbeat: state persist failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }
}
