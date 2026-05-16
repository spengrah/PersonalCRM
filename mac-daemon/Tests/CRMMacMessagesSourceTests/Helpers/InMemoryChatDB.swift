// InMemoryChatDB — builds a GRDB DatabaseQueue from the committed
// chat_db_schema.sql fixture, ready for test-specific INSERTs.
//
// The schema script lives in Fixtures/ and is copied as a resource on
// build. Tests load the script via Bundle.module.url(forResource:) and
// execute it once per-test inside a fresh in-memory database.
import Foundation
import GRDB
@testable import CRMMacMessagesSource

enum InMemoryChatDB {
    /// Resource bundle for CRMMacMessagesSourceTests. SwiftPM generates
    /// `Bundle.module` because the test target declares
    /// `resources: [.copy("Fixtures")]`.
    static func makeQueue(file: StaticString = #filePath, line: UInt = #line) throws -> DatabaseQueue {
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "chat_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw NSError(domain: "InMemoryChatDB", code: 1,
                          userInfo: [NSLocalizedDescriptionKey:
                                     "chat_db_schema.sql not found in Bundle.module/Fixtures"])
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let queue = try DatabaseQueue()
        try queue.write { db in
            // Execute the multi-statement schema script.
            try db.execute(sql: script)
        }
        return queue
    }

    /// Apple-epoch nanoseconds for a given UNIX timestamp.
    static func appleEpochNanos(unix: TimeInterval) -> Int64 {
        let seconds = unix - 978_307_200 // 2001-01-01 UTC
        return Int64(seconds * 1e9)
    }
}
