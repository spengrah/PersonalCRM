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

    /// Post-sign verification — mirrors the belt-and-suspenders block
    /// in `Scripts/assemble_bundle.sh`: `codesign --verify --strict
    /// --deep`, identifier match (inner + outer), and (cert mode
    /// only) the designated requirement is non-empty + not
    /// cdhash-anchored for both inner Mach-O and outer bundle.
    func verifyBundleCodesign(bundlePath: String, identifier: String) throws {
        try runCodesign(
            arguments: ["--verify", "--strict", "--deep", bundlePath],
            errorPrefix: "verify ")

        let innerMachoPath = "\(bundlePath)/\(BundleAssembler.machoRelativePath)"
        try assertIdentifierMatches(
            path: innerMachoPath, expected: identifier, label: "inner mach-o")
        try assertIdentifierMatches(
            path: bundlePath, expected: identifier, label: "outer bundle")

        guard signingIdentity != nil else { return }
        // Mirror the shell guard: BOTH inner and outer must have
        // non-empty, non-cdhash DR. A future refactor that breaks the
        // inner-pass `--identifier` / `--sign` argv could yield a
        // cdhash inner DR despite the outer being cert-leaf-anchored.
        try assertDesignatedRequirementNotCdhash(
            path: innerMachoPath, label: "inner mach-o")
        try assertDesignatedRequirementNotCdhash(
            path: bundlePath, label: "outer bundle")
    }

    private func assertIdentifierMatches(
        path: String, expected: String, label: String
    ) throws {
        let output = try runCodesignCapture(
            arguments: ["--display", "--verbose=2", path],
            errorPrefix: "\(label) display ")
        guard let actual = Self.parseIdentifier(from: output) else {
            throw ExecutableAdapterError.codesignFailed(
                "\(label) display: no Identifier line in codesign output")
        }
        if actual != expected {
            throw ExecutableAdapterError.codesignFailed(
                "\(label) identifier mismatch: got '\(actual)', expected '\(expected)'")
        }
    }

    private func assertDesignatedRequirementNotCdhash(
        path: String, label: String
    ) throws {
        let output = try runCodesignCapture(
            arguments: ["--display", "-r", "-", path],
            errorPrefix: "\(label) display DR ")
        let dr = Self.parseDesignatedRequirement(from: output)
        if dr.isEmpty {
            throw ExecutableAdapterError.codesignFailed(
                "\(label): could not parse designated requirement for \(path)")
        }
        if dr.contains("cdhash") {
            throw ExecutableAdapterError.codesignFailed(
                "\(label): cert-backed signing produced a cdhash designated requirement: \(dr)")
        }
    }

    /// Pure parser: extract the `Identifier=` value from `codesign
    /// --display --verbose=2` output. Returns nil if the marker line
    /// is absent. Exposed for unit-testing the no-line / trailing-
    /// whitespace / CR-line-ending edge cases without spawning real
    /// codesign.
    static func parseIdentifier(from output: String) -> String? {
        for rawLine in output.split(separator: "\n", omittingEmptySubsequences: false) {
            let line = rawLine.trimmingCharacters(in: .whitespacesAndNewlines)
            guard line.hasPrefix("Identifier=") else { continue }
            return String(line.dropFirst("Identifier=".count))
                .trimmingCharacters(in: .whitespacesAndNewlines)
        }
        return nil
    }

    /// Pure parser: extract the designated-requirement text from
    /// `codesign --display -r -` output. Returns an empty string if
    /// no `designated => ` line is found. Both `designated => ...`
    /// and `# designated => ...` appear in the wild across macOS
    /// versions; accept both.
    static func parseDesignatedRequirement(from output: String) -> String {
        for rawLine in output.split(separator: "\n", omittingEmptySubsequences: false) {
            let trimmed = rawLine.trimmingCharacters(in: .whitespacesAndNewlines)
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
