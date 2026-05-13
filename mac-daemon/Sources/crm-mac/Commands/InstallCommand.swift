// `crm-mac install` parses options and delegates to
// CRMMacLifecycle.Installer. The branchy validation lives in
// InstallRequestParser so it can be unit-tested without standing up
// the executable target (which has no test target by design).
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
        let input = InstallRequestParserInput(
            piURL: piURL,
            pair: pair,
            hostname: hostname,
            upgrade: upgrade,
            registerOnly: registerOnly)
        let request: InstallRequest
        do {
            request = try InstallRequestParser.parse(input)
        } catch let e as InstallRequestParseError {
            throw ValidationError(String(describing: e))
        }

        let ctx = ProductionContext()
        let installer = ctx.installer()
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
