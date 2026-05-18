// `crm-mac stop` halts the running daemon for the duration of an
// operator maintenance window. Delegates to
// CRMMacLifecycle.StopOps so the logic is testable against the
// FakeAgentService + FakeProcessSignaller.
import ArgumentParser
import Foundation
import CRMMacLifecycle

struct StopCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "stop",
        abstract: "Stop the running crm-mac daemon (for backfill/scan/maintenance ops).")

    @Option(name: .long, help: "Stop-window timeout in seconds. Default 10.")
    var timeout: Double = 10

    mutating func run() async throws {
        let ctx = ProductionContext()
        let result = await StopOps.run(
            StopOpsDependencies(
                paths: ctx.paths,
                filesystem: ctx.filesystem,
                agentService: ctx.agentService,
                processSignaller: ctx.processSignaller,
                logger: ctx.logger),
            timeoutSeconds: timeout)
        print("stopped=\(result.stopped)")
        print("pid=\(result.pid)")
        print("unregister_invoked=\(result.unregisterInvoked)")
        if !result.stopped {
            FileHandle.standardError.write(Data(
                "daemon did not exit within \(Int(timeout))s. Investigate: `ps -p \(result.pid)`. Manual recovery: `kill -9 \(result.pid)`.\n".utf8))
            throw ExitCode(1)
        }
    }
}
