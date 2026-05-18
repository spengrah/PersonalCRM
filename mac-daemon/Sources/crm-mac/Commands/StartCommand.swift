// `crm-mac start` re-registers the agent after `crm-mac stop`.
// Delegates to CRMMacLifecycle.StartOps so the logic is testable
// against FakeAgentService.
import ArgumentParser
import Foundation
import CRMMacLifecycle

struct StartCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "start",
        abstract: "Re-register the crm-mac agent after a maintenance stop.")

    @Option(name: .long, help: "Status-poll timeout (seconds) waiting for .enabled. Default 5.")
    var statusPollTimeout: Double = 5

    mutating func run() async throws {
        let ctx = ProductionContext()
        let result: StartOpsResult
        do {
            result = try await StartOps.run(
                StartOpsDependencies(
                    agentService: ctx.agentService,
                    logger: ctx.logger),
                statusPollTimeoutSeconds: statusPollTimeout)
        } catch let err as StartOpsError {
            FileHandle.standardError.write(Data("\(err)\n".utf8))
            throw ExitCode(1)
        }

        let outcomeStr: String
        switch result.outcome {
        case .registered:        outcomeStr = "registered"
        case .alreadyRegistered: outcomeStr = "already_registered"
        }
        print("register_outcome=\(outcomeStr)")
        print("status=\(result.finalStatus.rawValue)")
        print("started=\(result.started)")
        if result.started {
            return
        }
        switch result.finalStatus {
        case .requiresApproval:
            FileHandle.standardError.write(Data(
                "agent requires approval. Open System Settings → General → Login Items → Allow in Background → enable crm-mac, then re-run `crm-mac start`.\n".utf8))
        case .notRegistered:
            FileHandle.standardError.write(Data(
                "register call returned no error but status is not_registered. Check `crm-mac doctor` and Console.app SMAppService logs.\n".utf8))
        case .notFound:
            FileHandle.standardError.write(Data(
                "bundle missing at \(ctx.paths.bundleAppPath); run `crm-mac install --upgrade`.\n".utf8))
        case .enabled:
            break
        }
        throw ExitCode(1)
    }
}
