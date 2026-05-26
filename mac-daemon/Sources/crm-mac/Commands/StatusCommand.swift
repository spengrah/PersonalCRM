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
        print("registration_status=\(report.registrationStatus.rawValue)")
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

        if let phoneCalls = report.phoneCalls {
            print("  phone_calls:")
            let formatter = ISO8601DateFormatter()
            let liveDate = phoneCalls.liveCursorZDate
                .map { formatter.string(from: $0) } ?? "nil"
            let backfillDate = phoneCalls.backfillCursorZDate
                .map { formatter.string(from: $0) } ?? "nil"
            let installMax = phoneCalls.installMaxZDate
                .map { formatter.string(from: $0) } ?? "nil"
            print("    live_cursor:        \(liveDate) (Z_PK=\(phoneCalls.liveCursorZPK.map(String.init) ?? "nil"))")
            print("    backfill_cursor:    \(backfillDate) (Z_PK=\(phoneCalls.backfillCursorZPK.map(String.init) ?? "nil"))")
            print("    install_max:        \(installMax) (Z_PK=\(phoneCalls.installMaxZPK.map(String.init) ?? "nil"))")
            print("    backfill_complete:  \(phoneCalls.backfillComplete)")
        } else {
            print("  phone_calls: (no cursor committed yet)")
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

        renderAnarlog("anarlog_humans", report.anarlogHumans)
        renderAnarlog("anarlog_sessions", report.anarlogSessions)
    }

    private func renderAnarlog(_ name: String, _ status: AnarlogSourceStatus?) {
        guard let s = status else {
            print("  \(name): (no anarlog config; run `crm-mac configure anarlog --help`)")
            return
        }
        print("  \(name):")
        print("    enabled:            \(s.enabled)")
        let formatter = ISO8601DateFormatter()
        print("    last_scheduled_at:  \(s.lastScheduledAt.map { formatter.string(from: $0) } ?? "never")")
        print("    last_pushed_at:     \(s.lastPushedAt.map { formatter.string(from: $0) } ?? "never")")
        // cursorUUIDCount is nil for two distinct reasons:
        //   1. no state slot yet (source never ticked) → render as
        //      "never"
        //   2. state present but cursor JSON is malformed → render as
        //      "(decode_error)"
        // last_scheduled_at being nil tells us which case we're in
        // (state.lastScheduledAt is bumped at the TOP of every tick,
        // before any decode can happen).
        let countRendering: String
        if s.cursorUUIDCount == nil && s.lastScheduledAt == nil {
            countRendering = "never"
        } else if let n = s.cursorUUIDCount {
            countRendering = String(n)
        } else {
            countRendering = "(decode_error)"
        }
        print("    cursor_uuid_count:  \(countRendering)")
        print("    schema_version:     \(s.schemaVersion)")
        if s.recoveryRequested {
            print("    recovery_requested: YES (\(s.lastError ?? "unknown reason"))")
        } else if let err = s.lastError {
            print("    last_error:         \(err)")
        }
    }
}
