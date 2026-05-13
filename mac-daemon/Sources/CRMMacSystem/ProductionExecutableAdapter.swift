// ProductionExecutableAdapter: Bundle.main.executablePath +
// /usr/bin/codesign shell-out. Tested manually only — in CI the in-
// memory fake provides coverage.
import Foundation
import CRMMacLifecycle

public struct ProductionExecutableAdapter: ExecutableAdapter {
    public let codesignPath: String
    public init(codesignPath: String = "/usr/bin/codesign") {
        self.codesignPath = codesignPath
    }

    public func currentExecutablePath() throws -> String {
        guard let path = Bundle.main.executablePath else {
            throw ExecutableAdapterError.notFound
        }
        return path
    }

    public func adhocCodesign(path: String) throws {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: codesignPath)
        proc.arguments = [
            "-s", "-",
            "--force",
            "--preserve-metadata=identifier,entitlements,flags,runtime",
            path,
        ]
        let errPipe = Pipe()
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            throw ExecutableAdapterError.codesignFailed("spawn: \(error.localizedDescription)")
        }
        proc.waitUntilExit()
        if proc.terminationStatus != 0 {
            let stderr = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
            throw ExecutableAdapterError.codesignFailed("exit \(proc.terminationStatus): \(stderr)")
        }
    }
}
