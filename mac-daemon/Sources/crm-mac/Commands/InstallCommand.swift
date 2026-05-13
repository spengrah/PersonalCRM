// `crm-mac install` parses options and delegates to
// CRMMacLifecycle.Installer.
import Foundation
import ArgumentParser
import CRMMacLifecycle

struct InstallCommand: AsyncParsableCommand {
    static var configuration = CommandConfiguration(
        commandName: "install",
        abstract: "Pair with the Pi and register the launchd agent.")

    @Option(name: .long, help: "Pi base URL (e.g. https://pi.example.ts.net)")
    var piURL: String = ""

    @Option(name: .long, help: "Single-use pairing token from `crm-admin --mint-pairing-token` on the Pi")
    var pair: String = ""

    @Option(name: .long, help: "Non-PII hostname label (e.g. mac-1). REQUIRED on first install.")
    var hostname: String = ""

    @Flag(name: .long, help: "Re-install the binary in place. Does NOT re-pair.")
    var upgrade: Bool = false

    @Flag(name: .long, help: "Re-register an existing install with launchd. Does NOT replace the binary or re-pair.")
    var registerOnly: Bool = false

    mutating func run() async throws {
        if upgrade && registerOnly {
            throw ValidationError("--upgrade and --register-only are mutually exclusive")
        }
        // Per plan D9: --hostname is required only on a fresh install.
        if !upgrade && !registerOnly {
            if piURL.isEmpty {
                throw ValidationError("--pi-url <url> is required for fresh install")
            }
            if pair.isEmpty {
                throw ValidationError("--pair <token> is required for fresh install")
            }
            if hostname.isEmpty {
                throw ValidationError("--hostname <label> is required. Pick a non-PII label like 'mac-1', 'work-mac', 'home-laptop'.")
            }
        }

        let url: URL
        if piURL.isEmpty {
            url = URL(string: "https://localhost")!
        } else {
            guard let parsed = URL(string: piURL) else {
                throw ValidationError("--pi-url is not a valid URL: \(piURL)")
            }
            url = parsed
        }
        let ctx = ProductionContext()
        let installer = ctx.installer()
        let request = InstallRequest(
            piURL: url,
            pairingToken: pair,
            hostname: hostname,
            upgrade: upgrade,
            registerOnly: registerOnly)
        let summary = try await installer.run(request)
        print("install complete")
        print("  host_id=\(summary.hostID.uuidString.lowercased())")
        print("  binary=\(summary.binaryPath)")
        print("  plist=\(summary.plistPath)")
        print("")
        print("First launch on this Mac may be blocked by Gatekeeper. If so:")
        print("  System Settings -> Privacy & Security -> scroll to \"crm-mac was blocked...\" -> Open Anyway")
    }
}
