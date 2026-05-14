// `crm-mac uninstall` parses --purge and delegates to
// CRMMacLifecycle.Uninstaller.
import ArgumentParser
import CRMMacLifecycle

struct UninstallCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "uninstall",
        abstract: "Remove the launchd agent, plist, and Keychain entry. --purge also removes config and state.")

    @Flag(name: .long, help: "Also remove config.json, state.json, and the installed binary.")
    var purge: Bool = false

    mutating func run() throws {
        let ctx = ProductionContext()
        let summary = try ctx.uninstaller().run(UninstallRequest(purge: purge))
        print("uninstall complete")
        print("  bootout_invoked=\(summary.bootoutInvoked)")
        print("  bootout_exit_code=\(summary.bootoutExitCode)")
        print("  plist_deleted=\(summary.plistDeleted)")
        print("  keychain_deleted=\(summary.keychainDeleted)")
        print("  purged=\(summary.purged)")
        if !purge {
            print("")
            print("Run `crm-mac uninstall --purge` to also delete config.json + state.json + binary.")
        }
    }
}
