import Foundation
@testable import CRMMacAnarlogSource

struct FakeAnarlogResponse: Codable, Equatable {
    var argv: [String]
    var exit: Int32
    var stdout: String
    var stderr: String
    var delayMs: Int

    enum CodingKeys: String, CodingKey {
        case argv
        case exit
        case stdout
        case stderr
        case delayMs = "delay_ms"
    }

    init(argv: [String], exit: Int32 = 0, stdout: String = "",
         stderr: String = "", delayMs: Int = 0) {
        self.argv = argv
        self.exit = exit
        self.stdout = stdout
        self.stderr = stderr
        self.delayMs = delayMs
    }
}

struct FakeAnarlogScenario: Codable, Equatable {
    var responses: [FakeAnarlogResponse]
}

struct FakeAnarlogListEntry {
    let id: String
    let createdAt: String
}

struct FakeAnarlogParticipant {
    let humanID: String
    let displayName: String?
    let email: String?
    let jobTitle: String?
}

struct FakeAnarlogRecord {
    let id: String
    let title: String?
    let createdAt: String
    let noteMarkdown: String?
    let summaries: [String]
    let participants: [FakeAnarlogParticipant]
}

final class FakeAnarlogCLI {
    static let executablePath: String = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()
        .appendingPathComponent("fake-anarlog")
        .path

    let directory: URL

    init() throws {
        directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("fake-anarlog-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(
            at: directory, withIntermediateDirectories: true)
    }

    deinit {
        try? FileManager.default.removeItem(at: directory)
    }

    func setScenario(_ scenario: FakeAnarlogScenario) throws {
        let bytes = try JSONEncoder().encode(scenario)
        try bytes.write(to: directory.appendingPathComponent("scenario.json"), options: .atomic)
    }

    var environment: [String: String] {
        var result = ProcessInfo.processInfo.environment
        result["FAKE_ANARLOG_SCENARIO"] = directory.appendingPathComponent("scenario.json").path
        result["FAKE_ANARLOG_INVOCATIONS"] = directory.appendingPathComponent("invocations.jsonl").path
        return result
    }

    func invocations() throws -> [[String]] {
        let log = directory.appendingPathComponent("invocations.jsonl")
        guard FileManager.default.fileExists(atPath: log.path) else { return [] }
        let contents = try String(contentsOf: log, encoding: .utf8)
        return try contents.split(whereSeparator: \.isNewline).map { line in
            try JSONDecoder().decode([String].self, from: Data(line.utf8))
        }
    }

    func makeClient(timeout: TimeInterval = 10) -> AnarlogCLIProcessClient {
        AnarlogCLIProcessClient(
            cliPath: Self.executablePath,
            homeDirectory: directory,
            environment: environment,
            timeout: timeout)
    }

    static func listArgv(offset: Int) -> [String] {
        ["meetings", "--source", "local", "list", "--json",
         "--limit", "200", "--offset", String(offset)]
    }

    static func getArgv(id: String) -> [String] {
        ["meetings", "--source", "local", "get", "--json", id]
    }

    static func listStdout(entries: [FakeAnarlogListEntry], offset: Int,
                           nextOffset: Int?) -> String {
        let root: [String: Any] = [
            "schema_version": "1",
            "command": "meetings list",
            "data": entries.map { ["id": $0.id, "created_at": $0.createdAt] },
            "pagination": [
                "offset": offset,
                "limit": 200,
                "returned": entries.count,
                "total": NSNull(),
                "next_offset": nextOffset as Any? ?? NSNull(),
            ],
        ]
        return encode(root)
    }

    static func getStdout(_ record: FakeAnarlogRecord) -> String {
        let participantObjects: [[String: Any]] = record.participants.map { participant in
            [
                "human_id": participant.humanID,
                "display_name": participant.displayName as Any? ?? NSNull(),
                "email": participant.email as Any? ?? NSNull(),
                "job_title": participant.jobTitle as Any? ?? NSNull(),
            ]
        }
        let root: [String: Any] = [
            "schema_version": "1",
            "command": "meetings get",
            "data": [
                "id": record.id,
                "title": record.title as Any? ?? NSNull(),
                "created_at": record.createdAt,
                "note": record.noteMarkdown.map { ["markdown": $0] as Any } ?? NSNull(),
                "summaries": record.summaries.map {
                    ["kind": "summary", "markdown": $0]
                },
                "participants": participantObjects,
            ],
        ]
        return encode(root)
    }

    static func errorStderr(code: String, exitCode: Int32) -> String {
        encode([
            "schema_version": "1",
            "error": [
                "code": code,
                "exit_code": exitCode,
                "message": "synthetic",
            ],
        ])
    }

    private static func encode(_ value: [String: Any]) -> String {
        let data = try! JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
        return String(data: data, encoding: .utf8)!
    }
}
