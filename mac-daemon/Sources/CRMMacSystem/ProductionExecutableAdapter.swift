// ProductionExecutableAdapter: Bundle.main.executablePath +
// /usr/bin/codesign shell-out. Tested manually only — in CI the in-
// memory fake provides coverage.
import Foundation
import CRMMacLifecycle

public struct ProductionExecutableAdapter: ExecutableAdapter {
    public let codesignPath: String
    public let signingIdentity: String?

    public init(
        codesignPath: String = "/usr/bin/codesign",
        signingIdentity: String? = ProcessInfo.processInfo.environment["CRM_MAC_CODESIGN_IDENTITY"]
    ) {
        self.codesignPath = codesignPath
        let trimmed = signingIdentity?.trimmingCharacters(in: .whitespacesAndNewlines)
        if let trimmed, !trimmed.isEmpty, trimmed != "-" {
            self.signingIdentity = trimmed
        } else {
            self.signingIdentity = nil
        }
    }

    public func currentExecutablePath() throws -> String {
        guard let path = Bundle.main.executablePath else {
            throw ExecutableAdapterError.notFound
        }
        return path
    }

    public func adhocCodesign(path: String) throws {
        try runCodesign(
            arguments: [
                "-s", "-",
                "--force",
                "--preserve-metadata=identifier,entitlements,flags,runtime",
                path,
            ],
            errorPrefix: "")
    }

    public func codesignBundle(bundlePath: String, identifier: String) throws {
        // Pass 1: inner Mach-O with an explicit --identifier. This
        // overrides whatever identifier Swift's build pipeline embedded
        // (typically the executable name + a build-host-derived
        // suffix) so the codesign-recorded identifier matches the bundle ID.
        let innerMachoPath = "\(bundlePath)/\(BundleAssembler.machoRelativePath)"
        try runCodesign(
            arguments: signingArguments(identifier: identifier) + [
                "--force",
                innerMachoPath,
            ],
            errorPrefix: "inner mach-o ")
        // Pass 2: bundle seal (no --deep; the inner binary was signed
        // in pass 1. --deep is officially discouraged by Apple for
        // signing and reserved for VERIFY-only).
        try runCodesign(
            arguments: signingArguments(identifier: identifier) + [
                "--force",
                bundlePath,
            ],
            errorPrefix: "bundle seal ")
    }

    private func signingArguments(identifier: String) -> [String] {
        var args = [
            "--sign", signingIdentity ?? "-",
            "--identifier", identifier,
        ]
        if signingIdentity != nil {
            args.append("--timestamp=none")
        }
        return args
    }

    private func runCodesign(arguments: [String], errorPrefix: String) throws {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: codesignPath)
        proc.arguments = arguments
        let errPipe = Pipe()
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            throw ExecutableAdapterError.codesignFailed(
                "\(errorPrefix)spawn: \(error.localizedDescription)")
        }
        proc.waitUntilExit()
        if proc.terminationStatus != 0 {
            let stderr = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
            throw ExecutableAdapterError.codesignFailed(
                "\(errorPrefix)exit \(proc.terminationStatus): \(stderr)")
        }
    }
}
