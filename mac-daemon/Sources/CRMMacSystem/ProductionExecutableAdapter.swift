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
        // Post-sign verification — mirrors the belt-and-suspenders
        // block in Scripts/assemble_bundle.sh so the install-time path
        // catches the same silent-failure modes (identifier drift,
        // ad-hoc-mode DR despite an identity being set, codesign
        // output corruption). Without this, a future refactor that
        // drops `--identifier` from `signingArguments` would let
        // installs complete "successfully" while TCC quietly broke.
        try verifyBundleCodesign(bundlePath: bundlePath, identifier: identifier)
    }

    /// Post-sign verification — three checks: codesign --verify
    /// integrity, identifier match (inner + outer), and (cert mode
    /// only) the designated requirement is not cdhash-anchored and
    /// not empty.
    func verifyBundleCodesign(bundlePath: String, identifier: String) throws {
        try runCodesign(
            arguments: ["--verify", "--strict", bundlePath],
            errorPrefix: "verify ")

        let innerMachoPath = "\(bundlePath)/\(BundleAssembler.machoRelativePath)"
        try assertIdentifierMatches(
            path: innerMachoPath, expected: identifier, label: "inner mach-o")
        try assertIdentifierMatches(
            path: bundlePath, expected: identifier, label: "outer bundle")

        guard signingIdentity != nil else { return }
        let dr = try displayDesignatedRequirement(path: bundlePath)
        if dr.isEmpty {
            throw ExecutableAdapterError.codesignFailed(
                "could not parse designated requirement for \(bundlePath)")
        }
        if dr.contains("cdhash") {
            throw ExecutableAdapterError.codesignFailed(
                "cert-backed signing produced a cdhash designated requirement: \(dr)")
        }
    }

    private func assertIdentifierMatches(
        path: String, expected: String, label: String
    ) throws {
        let output = try runCodesignCapture(
            arguments: ["--display", "--verbose=2", path],
            errorPrefix: "\(label) display ")
        for rawLine in output.split(separator: "\n") {
            let line = String(rawLine)
            guard line.hasPrefix("Identifier=") else { continue }
            let actual = String(line.dropFirst("Identifier=".count))
            if actual != expected {
                throw ExecutableAdapterError.codesignFailed(
                    "\(label) identifier mismatch: got '\(actual)', expected '\(expected)'")
            }
            return
        }
        throw ExecutableAdapterError.codesignFailed(
            "\(label) display: no Identifier line in codesign output")
    }

    private func displayDesignatedRequirement(path: String) throws -> String {
        let output = try runCodesignCapture(
            arguments: ["--display", "-r", "-", path],
            errorPrefix: "display DR ")
        for rawLine in output.split(separator: "\n") {
            let trimmed = rawLine.trimmingCharacters(in: .whitespaces)
            // codesign output format has shifted across macOS versions;
            // both `designated => ...` and `# designated => ...` appear
            // in the wild. Accept both.
            if trimmed.hasPrefix("designated => ") {
                return String(trimmed.dropFirst("designated => ".count))
            }
            if trimmed.hasPrefix("# designated => ") {
                return String(trimmed.dropFirst("# designated => ".count))
            }
        }
        return ""
    }

    /// Like `runCodesign` but captures and returns merged stdout+stderr.
    /// `codesign --display` writes to stderr by default; `--display -r -`
    /// writes to stdout on newer macOS. Merging covers both.
    private func runCodesignCapture(
        arguments: [String], errorPrefix: String
    ) throws -> String {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: codesignPath)
        proc.arguments = arguments
        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            throw ExecutableAdapterError.codesignFailed(
                "\(errorPrefix)spawn: \(error.localizedDescription)")
        }
        proc.waitUntilExit()
        let stdout = String(data: outPipe.fileHandleForReading.readDataToEndOfFile(),
                            encoding: .utf8) ?? ""
        let stderr = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(),
                            encoding: .utf8) ?? ""
        if proc.terminationStatus != 0 {
            throw ExecutableAdapterError.codesignFailed(
                "\(errorPrefix)exit \(proc.terminationStatus): \(stderr)")
        }
        return stdout + "\n" + stderr
    }

    /// Builds the leading codesign argument list for the current
    /// `signingIdentity`. Exposed as `internal` so the test target can
    /// assert that ad-hoc and cert-backed modes produce distinct argv.
    func signingArguments(identifier: String) -> [String] {
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
