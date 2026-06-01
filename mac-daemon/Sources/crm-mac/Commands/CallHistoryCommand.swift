// crm-mac call-history — operator ops subcommands for the phone_calls
// source.
//
// Top-level:
//   crm-mac call-history backfill --restart [--yes]
//   crm-mac call-history scan --identifier <handle> [--since <duration>]
//   crm-mac call-history status
//
// `backfill` + `scan` refuse to run while the daemon is up (pidfile-lock
// guard) and commit Pi-side via the GET -> mutate -> POST CAS flow.
// `status` is a read-only convenience that mirrors the phone_calls
// slice of `crm-mac status` for callers who only want the source-
// specific lines.
//
// A debug `dump --limit N` subcommand is intentionally deferred to a
// follow-up — it would need to open CallHistoryDB read-only with FDA
// granted, which the support workflow rarely needs (the Pi-side
// staging table covers most "what did the daemon see" questions).
import Foundation
import ArgumentParser
import CRMMacCore
import CRMMacLifecycle
import CRMMacPiClient
import CRMMacSystem

struct CallHistoryCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "call-history",
        abstract: "Operator commands for the Phone & FaceTime call-history source.",
        subcommands: [
            CallHistoryBackfillCommand.self,
            CallHistoryScanCommand.self,
            CallHistoryStatusCommand.self,
        ])
}

struct CallHistoryStatusCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "status",
        abstract: "Print the phone_calls source's cursor + backfill state.")

    private func formatAppleEpoch(_ value: Double?) -> String {
        guard let v = value else { return "nil" }
        let date = Date(timeIntervalSince1970: 978_307_200 + v)
        return ISO8601DateFormatter().string(from: date)
    }

    mutating func run() throws {
        let ctx = ProductionContext()
        let report = ctx.status().run()
        if let phoneCalls = report.phoneCalls {
            let liveDate = formatAppleEpoch(phoneCalls.liveCursorZDate)
            let backfillDate = formatAppleEpoch(phoneCalls.backfillCursorZDate)
            let installMax = formatAppleEpoch(phoneCalls.installMaxZDate)
            print("live_cursor=\(liveDate) (Z_PK=\(phoneCalls.liveCursorZPK.map(String.init) ?? "nil"))")
            print("backfill_cursor=\(backfillDate) (Z_PK=\(phoneCalls.backfillCursorZPK.map(String.init) ?? "nil"))")
            print("install_max=\(installMax) (Z_PK=\(phoneCalls.installMaxZPK.map(String.init) ?? "nil"))")
            print("backfill_complete=\(phoneCalls.backfillComplete)")
            print("pending_scans=\(phoneCalls.pendingScansCount)")
        } else {
            print("(no cursor committed yet)")
        }
    }
}

struct CallHistoryBackfillCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "backfill",
        abstract: "Reset the phone_calls cursor (re-walk historical calls).")

    @Flag(name: .long,
          help: "Required: reset the phone_calls cursor to install-time state.")
    var restart: Bool = false

    @Flag(name: .long,
          help: "Skip the confirmation prompt. For scripted recovery.")
    var yes: Bool = false

    mutating func run() async throws {
        if !restart {
            throw ValidationError("--restart is required")
        }
        let ops = try makeCallHistoryOps()
        do {
            try await ops.backfillRestart(yes: yes)
            print("phone_calls cursor reset; the daemon's next tick will recompute install-time MAX(ZDATE, Z_PK).")
        } catch let err as CallHistoryOpsError {
            FileHandle.standardError.write(Data("\(err.description)\n".utf8))
            throw ExitCode(4)
        }
    }
}

struct CallHistoryScanCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "scan",
        abstract: "Queue a backwards scan for a specific identifier.")

    @Option(name: .long, help: "Phone or email handle to scan for. Normalized before queuing.")
    var identifier: String

    @Option(name: .long,
             help: "Scan window as <number><unit> where unit is s/m/h/d (e.g. '30d'); default 30d.")
    var since: String = "30d"

    mutating func run() async throws {
        let seconds = try parseSince(since)
        let ops = try makeCallHistoryOps()
        do {
            try await ops.scan(identifier: identifier, since: seconds)
            print("scan queued for \(identifier) (since=\(since)). Start the daemon to drain.")
        } catch let err as CallHistoryOpsError {
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

/// Construct a CallHistoryOps with production wiring.
private func makeCallHistoryOps() throws -> CallHistoryOps {
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
    let backfillFloor = PhoneCallsCursorWire.defaultBackfillFloor

    return CallHistoryOps(
        piClient: piClient,
        auth: auth,
        pidfileLock: pidfileLock,
        logger: logger,
        backfillFloor: backfillFloor)
}
