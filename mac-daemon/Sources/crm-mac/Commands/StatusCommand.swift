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
    }
}
