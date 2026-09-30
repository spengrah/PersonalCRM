import CRMMacCore
import Darwin
import Foundation

public struct AnarlogCLIProcessClient: AnarlogCLIClient {
    public static let defaultTimeout: TimeInterval = 30
    public static let pageSize = 200
    private static let terminationGrace: TimeInterval = 1

    private let cliPath: String?
    private let homeDirectory: URL
    private let environment: [String: String]
    private let timeout: TimeInterval

    public init(
        cliPath: String?,
        homeDirectory: URL = FileManager.default.homeDirectoryForCurrentUser,
        environment: [String: String] = ProcessInfo.processInfo.environment,
        timeout: TimeInterval = AnarlogCLIProcessClient.defaultTimeout
    ) {
        self.cliPath = cliPath
        self.homeDirectory = homeDirectory
        self.environment = environment
        self.timeout = timeout
    }

    public func listSessions() async throws(AnarlogCLIFailure) -> [AnarlogSessionListEntry] {
        var offset = 0
        var result: [AnarlogSessionListEntry] = []
        var seenIDs = Set<String>()

        while true {
            let arguments = [
                "meetings", "--source", "local", "list", "--json",
                "--limit", String(Self.pageSize), "--offset", String(offset),
            ]
            guard let stdout = try invoke(
                arguments: arguments,
                command: "meetings list",
                notFoundReturnsNil: false) else {
                return result
            }
            let page = try AnarlogCLIDecoder.listPage(stdout)
            for entry in page.entries where seenIDs.insert(entry.id).inserted {
                result.append(entry)
            }
            guard let nextOffset = page.nextOffset else { return result }
            guard nextOffset > offset else {
                throw .malformedOutput(command: "meetings list")
            }
            offset = nextOffset
        }
    }

    public func getSession(id: String) async throws(AnarlogCLIFailure) -> AnarlogSessionRecord? {
        let arguments = ["meetings", "--source", "local", "get", "--json", id]
        guard let stdout = try invoke(
            arguments: arguments,
            command: "meetings get",
            notFoundReturnsNil: true) else {
            return nil
        }
        return try AnarlogCLIDecoder.session(stdout)
    }

    private func invoke(
        arguments: [String],
        command: String,
        notFoundReturnsNil: Bool
    ) throws(AnarlogCLIFailure) -> Data? {
        guard let executable = AnarlogCLIExecutableResolver.resolve(
            cliPath: cliPath, homeDirectory: homeDirectory) else {
            throw .binaryNotFound
        }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        process.environment = environment
        process.standardInput = FileHandle.nullDevice

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe
        let drainGroup = DispatchGroup()
        let stdout = PipeDataCollector(fileHandle: stdoutPipe.fileHandleForReading)
        let stderr = PipeDataCollector(fileHandle: stderrPipe.fileHandleForReading)
        stdout.start(group: drainGroup)
        stderr.start(group: drainGroup)

        let terminated = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in terminated.signal() }
        let deadline = DispatchTime.now() + timeout
        do {
            try process.run()
        } catch {
            try? stdoutPipe.fileHandleForWriting.close()
            try? stderrPipe.fileHandleForWriting.close()
            stdout.stop()
            stderr.stop()
            throw .binaryNotFound
        }

        if terminated.wait(timeout: deadline) == .timedOut {
            terminate(process, terminated: terminated)
            stdout.stop()
            stderr.stop()
            throw .timeout
        }
        if drainGroup.wait(timeout: deadline) == .timedOut {
            terminate(process, terminated: terminated)
            stdout.stop()
            stderr.stop()
            throw .timeout
        }

        guard process.terminationReason == .exit else {
            throw .nonZeroExit(code: process.terminationStatus, errorCode: nil)
        }
        let status = process.terminationStatus
        if status == 0 {
            return stdout.data
        }

        let errorCode = try AnarlogCLIDecoder.errorCode(
            stderr: stderr.data, command: command)
        if status == 2, errorCode == "not_found" {
            if notFoundReturnsNil { return nil }
            throw .nonZeroExit(code: status, errorCode: errorCode)
        }
        if status == 3, errorCode == "database_not_found" {
            throw .databaseNotFound
        }
        throw .nonZeroExit(code: status, errorCode: errorCode)
    }

    private func terminate(_ process: Process, terminated: DispatchSemaphore) {
        guard process.isRunning else { return }

        process.terminate()
        if terminated.wait(timeout: .now() + Self.terminationGrace) == .timedOut,
           process.isRunning {
            _ = kill(process.processIdentifier, SIGKILL)
            _ = terminated.wait(timeout: .now() + Self.terminationGrace)
        }
    }
}

private final class PipeDataCollector: @unchecked Sendable {
    private let fileHandle: FileHandle
    private let lock = NSLock()
    private var contents = Data()
    private var group: DispatchGroup?
    private var isFinished = false

    init(fileHandle: FileHandle) {
        self.fileHandle = fileHandle
    }

    func start(group: DispatchGroup) {
        lock.lock()
        self.group = group
        group.enter()
        lock.unlock()

        fileHandle.readabilityHandler = { [weak self] handle in
            let available = handle.availableData
            if available.isEmpty {
                self?.finish()
            } else {
                self?.append(available)
            }
        }
    }

    func finish() {
        lock.lock()
        guard !isFinished else {
            lock.unlock()
            return
        }
        isFinished = true
        let group = self.group
        self.group = nil
        lock.unlock()

        fileHandle.readabilityHandler = nil
        group?.leave()
    }

    func stop() {
        finish()
        _ = try? fileHandle.close()
    }

    private func append(_ data: Data) {
        lock.lock()
        if !isFinished {
            contents.append(data)
        }
        lock.unlock()
    }

    var data: Data {
        lock.lock()
        defer { lock.unlock() }
        return contents
    }
}
