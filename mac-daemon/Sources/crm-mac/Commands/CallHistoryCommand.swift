// crm-mac call-history — operator ops subcommands for the phone_calls
// source.
//
// Top-level:
//   crm-mac call-history backfill --restart [--yes]
//
// Refuses to run while the daemon is up (pidfile-lock guard). Commits
// changes Pi-side via the GET -> mutate -> POST CAS flow; local
// state.json mirrors via the daemon's next tick.
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
        ])
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
