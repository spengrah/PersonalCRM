// InMemoryCallHistoryDB — builds a GRDB DatabaseQueue from the
// committed call_history_db_schema.sql fixture, ready for test-
// specific INSERTs. Mirrors InMemoryChatDB from
// CRMMacMessagesSourceTests.
import Foundation
import GRDB
@testable import CRMMacPhoneCallsSource

enum InMemoryCallHistoryDB {
    /// Resource bundle for CRMMacPhoneCallsSourceTests. SwiftPM
    /// generates `Bundle.module` because the test target declares
    /// `resources: [.copy("Fixtures")]`.
    static func makeQueue(file: StaticString = #filePath, line: UInt = #line) throws -> DatabaseQueue {
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "call_history_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw NSError(domain: "InMemoryCallHistoryDB", code: 1,
                          userInfo: [NSLocalizedDescriptionKey:
                                     "call_history_db_schema.sql not found in Bundle.module/Fixtures"])
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let queue = try DatabaseQueue()
        try queue.write { db in
            try db.execute(sql: script)
        }
        return queue
    }

    /// Apple-epoch SECONDS for a given UNIX timestamp.
    static func appleEpochSeconds(unix: TimeInterval) -> Double {
        unix - 978_307_200 // 2001-01-01 UTC
    }
}
