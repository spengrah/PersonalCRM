// `crm-mac uninstall` parses --purge and delegates to
// CRMMacLifecycle.Uninstaller. Async because Uninstaller.run() is
// async — agentService.unregister + pidfile-poll wait both need to
// await.
import ArgumentParser
import CRMMacLifecycle

struct UninstallCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "uninstall",
        abstract: "Unregister the agent, remove the bundle, and delete the api-key. --purge also removes config and state.")

    @Flag(name: .long, help: "Also remove config.json, state.json, logs, and the icloud hash cache.")
    var purge: Bool = false

    mutating func run() async throws {
        let ctx = ProductionContext()
        let summary = try await ctx.uninstaller().run(UninstallRequest(purge: purge))
        print("uninstall complete")
        print("  daemon_stopped=\(summary.daemonStopped)")
        print("  unregister_invoked=\(summary.unregisterInvoked)")
        print("  bundle_deleted=\(summary.bundleDeleted)")
        print("  keychain_deleted=\(summary.keychainDeleted)")
        print("  legacy_plist_deleted=\(summary.legacyPlistDeleted)")
        print("  legacy_binary_deleted=\(summary.legacyBinaryDeleted)")
        print("  purged=\(summary.purged)")
        if !purge {
            print("")
            print("Run `crm-mac uninstall --purge` to also delete config.json + state.json + logs.")
        }
    }
}
