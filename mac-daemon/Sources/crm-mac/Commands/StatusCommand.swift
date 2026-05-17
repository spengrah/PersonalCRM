// `crm-mac status` prints a compact summary of the daemon's
// installed-and-registered state plus the most recent heartbeat from
// state.json. NO network I/O.
import Foundation
import ArgumentParser
import CRMMacLifecycle

struct StatusCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "status",
        abstract: "Print the daemon's installation + heartbeat status.")

    mutating func run() throws {
        let ctx = ProductionContext()
        let report = ctx.status().run()
        print("installed=\(report.installed)")
        print("registered=\(report.registered)")
        if let hostname = report.configHostname {
            print("hostname=\(hostname)")
        }
        if let url = report.configPiURL {
            print("pi_url=\(url)")
        }
        if let id = report.hostID {
            print("host_id=\(id.uuidString.lowercased())")
        }
        if let v = report.stateSchemaVersion {
            print("state_schema_version=\(v)")
        }
        if let last = report.lastHeartbeatAt {
            let formatter = ISO8601DateFormatter()
            print("last_heartbeat_at=\(formatter.string(from: last))")
        } else {
            print("last_heartbeat_at=never")
        }

        // Sources block — each source surfaces its own watermarks.
        print("")
        print("Sources:")
        if let messages = report.messages {
            print("  messages:")
            print("    live_cursor:        \(messages.liveCursor.map(String.init) ?? "nil")")
            print("    backfill_cursor:    \(messages.backfillCursor.map(String.init) ?? "nil")")
            print("    install_max_rowid:  \(messages.installMaxRowID.map(String.init) ?? "nil")")
            print("    backfill_complete:  \(messages.backfillComplete)")
            print("    pending_scans:      \(messages.pendingScansCount)")
            if let installMax = messages.installMaxRowID, installMax > 0 {
                // Backfill descends from installMaxRowID toward 0;
                // progress = (start - current) / start, clamped to [0, 1].
                let cursor = messages.backfillCursor ?? installMax
                let raw = Double(installMax - cursor) / Double(installMax)
                let pct = Int(min(1.0, max(0.0, raw)) * 100)
                print("    backfill_progress:  ~\(pct)%")
            }
        } else {
            print("  messages: (no cursor committed yet)")
        }

        if let icloud = report.icloudContacts {
            print("  icloud_contacts:")
            print("    containers:         \(icloud.containerCount)")
            let formatter = ISO8601DateFormatter()
            let scheduled = icloud.lastScheduledAt
                .map { formatter.string(from: $0) } ?? "never"
            let pushed = icloud.lastPushedAt
                .map { formatter.string(from: $0) } ?? "never"
            print("    last_scheduled_at:  \(scheduled)")
            print("    last_pushed_at:     \(pushed)")
            if icloud.recoveryRequested {
                // Surface recovery flag prominently so operators
                // notice pending reconciliation. The reason follows
                // the colon (allowlist_changed, hash_mismatch, etc.).
                print("    recovery_requested: YES (\(icloud.lastError ?? "unknown reason"))")
            } else if let err = icloud.lastError {
                print("    last_error:         \(err)")
            }
        } else {
            print("  icloud_contacts: (no containers configured; run `crm-mac configure containers`)")
        }
    }
}
