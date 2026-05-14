// crm-mac messages — operator ops subcommands.
//
// Top-level:
//   crm-mac messages backfill --restart [--yes]
//   crm-mac messages scan --identifier <handle> [--since <duration>]
//
// Both refuse to run while the daemon is up (pidfile-lock guard).
// Both commit changes Pi-side via the GET -> mutate -> POST CAS flow;
// local state.json mirrors via the daemon's next tick.
import Foundation
import ArgumentParser
import CRMMacCore
import CRMMacLifecycle
import CRMMacMessagesSource
import CRMMacPiClient
import CRMMacSystem

struct MessagesCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "messages",
        abstract: "Operator commands for the Apple Messages source.",
        subcommands: [
            MessagesBackfillCommand.self,
            MessagesScanCommand.self,
        ])
}

struct MessagesBackfillCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "backfill",
        abstract: "Reset the messages cursor (re-walk historical messages).")

    @Flag(name: .long,
          help: "Required: reset the messages cursor to install-time state.")
    var restart: Bool = false

    @Flag(name: .long,
          help: "Skip the confirmation prompt. For scripted recovery.")
    var yes: Bool = false

    mutating func run() async throws {
        if !restart {
            throw ValidationError("--restart is required")
        }
        let ops = try makeMessagesOps()
        do {
            try await ops.backfillRestart(yes: yes)
            print("messages cursor reset; the daemon's next tick will recompute install-time MAX(ROWID).")
        } catch let err as MessagesOpsError {
            FileHandle.standardError.write(Data("\(err.description)\n".utf8))
            throw ExitCode(4)
        }
    }
}

struct MessagesScanCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "scan",
        abstract: "Queue a backwards scan for a specific identifier.")

    @Option(name: .long, help: "Phone or email handle to scan for. Normalized before queuing.")
    var identifier: String

    @Option(name: .long,
             help: "Scan window. ISO-8601 duration (e.g. '30d') or unset for default 30d.")
    var since: String = "30d"

    mutating func run() async throws {
        let seconds = try parseSince(since)
        let ops = try makeMessagesOps()
        do {
            try await ops.scan(identifier: identifier, since: seconds)
            print("scan queued for \(identifier) (since=\(since)). Start the daemon to drain.")
        } catch let err as MessagesOpsError {
            FileHandle.standardError.write(Data("\(err.description)\n".utf8))
            throw ExitCode(4)
        }
    }

    /// Parse a simple duration like "30d", "12h", "60m", "3600s".
    private func parseSince(_ raw: String) throws -> TimeInterval {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let unit = s.last else {
            throw ValidationError("invalid --since: \(raw)")
        }
        let numericPart = String(s.dropLast())
        guard let value = Double(numericPart), value >= 0 else {
            throw ValidationError("invalid --since: \(raw)")
        }
        switch unit {
        case "s": return value
        case "m": return value * 60
        case "h": return value * 60 * 60
        case "d": return value * 60 * 60 * 24
        default:
            throw ValidationError("invalid --since unit (expected s/m/h/d): \(raw)")
        }
    }
}

/// Construct a MessagesOps with production wiring.  Both subcommands
/// share this helper so the dependency wiring stays in one place.
private func makeMessagesOps() throws -> MessagesOps {
    let ctx = ProductionContext()
    let logger = ctx.logger

    let artifacts: DaemonStartupArtifacts
    do {
        artifacts = try DaemonStartup(
            paths: ctx.paths,
            keychain: ctx.keychain,
            logger: logger).run()
    } catch let startupErr as DaemonStartupError {
        throw ExitCode(startupErr.exitCode.rawValue)
    }

    let piClient = PiClient(baseURL: artifacts.config.piURL, logger: logger)
    let auth = PiAuth(hostID: artifacts.config.hostID, apiKey: artifacts.apiKey)
    let pidfileURL = URL(fileURLWithPath: ctx.paths.runtimeDirPath)
        .appendingPathComponent("daemon.pid")
    let pidfileLock = PidfileLock(path: pidfileURL)
    let backfillFloor = MessagesSourceConfig.defaultBackfillFloor

    return MessagesOps(
        piClient: piClient,
        auth: auth,
        pidfileLock: pidfileLock,
        logger: logger,
        backfillFloor: backfillFloor)
}
