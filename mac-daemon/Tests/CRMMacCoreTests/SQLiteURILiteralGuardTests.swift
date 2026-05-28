// SQLiteURILiteralGuardTests — a source-tree grep guard that forbids
// ad-hoc live-DB opens outside the single owner,
// `SQLiteSnapshotReader.swift`.
//
// Every SQLite file the daemon opens is a read-only tail of a live
// macOS store (chat.db, CallHistoryDB, future WhatsApp DBs). The local
// convention is "all GRDB file opens go through SQLiteSnapshotReader",
// so this guard forbids two regression shapes in production `Sources/`:
//
// Family 1 — forbidden SQLite-URI string literals (the original bug
//   class). A re-introduced `?mode=ro` literal, an `immutable=1` flag
//   (which would re-blind the reader to WAL-resident writes — the
//   production regression that motivated this guard), a `cache=shared`
//   flag (deliberately never used), or a bare `file://` SQLite URI
//   prefix anywhere in code other than the helper.
//
// Family 2 — forbidden bare-path live-DB opens. A future
//   `DatabasePool(path: config.whatsappDBPath.path, ...)` would slip
//   past a URI-only grep, so any file that constructs a
//   `DatabasePool(path:` or `DatabaseQueue(path:` MUST contain at least
//   as many `SQLiteSnapshotReader.readOnlyURI(` calls as it has opens.
//   This is file-level (not per-call dataflow) because the production
//   call sites bind the URI to a local first
//   (`let uri = SQLiteSnapshotReader.readOnlyURI(...); DatabasePool(path: uri, ...)`),
//   so a same-call-argument rule would reject our own correct code. The
//   call-count match (#opens <= #helper-calls) raises the bar from
//   "one helper call covers any number of opens" to "every open needs
//   its own helper call," which is sufficient for the single-DB-per-file
//   present and the multi-DB WhatsApp future. It is an accepted, bounded
//   limitation (a file with 2 opens + 2 helper calls whose wiring is
//   crossed would still pass); upgrade to a per-call dataflow check if a
//   future reader ever needs finer granularity.
//
// Comments are stripped (string-literal-aware) before scanning so the
// reader doc comments and the Pi-URL `file://` rejection comments in
// Config.swift / InstallRequestParser.swift do not trip the guard.
//
// Test targets are NOT scanned — only `Sources/` — so WAL-test writer
// configs that use DatabaseQueue/DatabasePool directly are fine.
import XCTest
@testable import CRMMacCore

final class SQLiteURILiteralGuardTests: XCTestCase {
    /// Only file that may own a Family-1 SQLite URI literal. Family 2
    /// needs no file allowlist — the per-file `readOnlyURI(` count is
    /// the exception mechanism.
    private static let family1AllowedFilenames: Set<String> = [
        "SQLiteSnapshotReader.swift",
    ]

    func testNoAdHocLiveDBOpensOutsideHelper() throws {
        let sourcesRoot = Self.resolveSourcesRoot()
        XCTAssertTrue(FileManager.default.fileExists(atPath: sourcesRoot.path),
                      "Sources root not found at \(sourcesRoot.path) — did the " +
                      "test file move relative to mac-daemon/Sources?")

        let swiftFiles = try Self.swiftFiles(under: sourcesRoot)
        XCTAssertFalse(swiftFiles.isEmpty, "expected to find .swift files under Sources/")

        var failures: [String] = []
        for fileURL in swiftFiles {
            let raw = try String(contentsOf: fileURL, encoding: .utf8)
            let code = Self.strippingComments(raw)
            let filename = fileURL.lastPathComponent

            // Family 1 — forbidden URI literals (skip the helper).
            if !Self.family1AllowedFilenames.contains(filename) {
                failures.append(contentsOf: Self.family1Failures(code: code, filename: filename))
            }

            // Family 2 — bare-path opens must route through the helper.
            failures.append(contentsOf: Self.family2Failures(code: code, filename: filename))
        }

        XCTAssertTrue(failures.isEmpty,
                      "ad-hoc live-DB open(s) found — route every SQLite file open " +
                      "through SQLiteSnapshotReader.readOnlyURI(for:):\n" +
                      failures.joined(separator: "\n"))
    }

    // MARK: - Family 1

    private static func family1Failures(code: String, filename: String) -> [String] {
        var out: [String] = []
        // `immutable=1` and `cache=shared` are forbidden anywhere in
        // code (belt-and-suspenders; the helper uses neither).
        if code.contains("immutable=1") {
            out.append("\(filename): `immutable=1` forbidden — it re-blinds the " +
                       "reader to WAL-resident writes (the production regression " +
                       "that motivated this guard).")
        }
        if code.contains("cache=shared") {
            out.append("\(filename): `cache=shared` forbidden — GRDB only honours it " +
                       "for in-memory DBs; never use it for a file-backed reader.")
        }
        // `mode=ro` only appears in a SQLite URI literal; forbid it
        // outside the helper.
        if code.contains("mode=ro") {
            out.append("\(filename): `mode=ro` URI literal forbidden outside " +
                       "SQLiteSnapshotReader.")
        }
        // A `file://` immediately followed by a `\(` interpolation OR
        // `?mode=ro` is the live-DB SQLite-URI shape. Scoping to that
        // shape avoids tripping on any future `file://`-scheme literal
        // that is NOT a SQLite open (none exist today). The
        // interpolation check is a plain `contains`: in stripped source
        // text, `file://\(...)` appears as the literal characters
        // f i l e : / / \ ( — so `"file://\\("` (backslash + paren) is
        // the exact byte sequence to find.
        if code.contains("file://\\(")
            || code.range(of: #"file://[^"\s]*\?mode=ro"#, options: .regularExpression) != nil {
            out.append("\(filename): `file://...` SQLite URI literal forbidden outside " +
                       "SQLiteSnapshotReader.")
        }
        return out
    }

    // MARK: - Family 2

    private static func family2Failures(code: String, filename: String) -> [String] {
        // Whitespace/newline-tolerant: a `DatabasePool(path:` or
        // `DatabaseQueue(path:` call may wrap across lines after the
        // open paren (`DatabasePool(\n    path: ...)`), so match the
        // constructor + the `path:` first argument across any
        // intervening whitespace.
        let openCount = Self.regexCount(#"DatabasePool\(\s*path:"#, in: code)
            + Self.regexCount(#"DatabaseQueue\(\s*path:"#, in: code)
        if openCount == 0 { return [] }
        let helperCount = Self.occurrences(of: "SQLiteSnapshotReader.readOnlyURI(", in: code)
        if helperCount < openCount {
            return ["\(filename): \(openCount) DatabasePool/Queue(path:) open(s) but only " +
                    "\(helperCount) SQLiteSnapshotReader.readOnlyURI(...) call(s) — every " +
                    "file DB open must route through the helper."]
        }
        return []
    }

    // MARK: - helpers

    /// Resolve `mac-daemon/Sources` from THIS file's compile-time path.
    /// File lives at mac-daemon/Tests/CRMMacCoreTests/<file>.swift, so
    /// three `deletingLastPathComponent()` reach mac-daemon/.
    private static func resolveSourcesRoot() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent() // CRMMacCoreTests/
            .deletingLastPathComponent() // Tests/
            .deletingLastPathComponent() // mac-daemon/
            .appendingPathComponent("Sources")
    }

    private static func swiftFiles(under root: URL) throws -> [URL] {
        var result: [URL] = []
        guard let enumerator = FileManager.default.enumerator(
            at: root,
            includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]) else {
            return result
        }
        for case let url as URL in enumerator where url.pathExtension == "swift" {
            result.append(url)
        }
        return result
    }

    private static func occurrences(of needle: String, in haystack: String) -> Int {
        guard !needle.isEmpty else { return 0 }
        var count = 0
        var searchRange = haystack.startIndex..<haystack.endIndex
        while let found = haystack.range(of: needle, range: searchRange) {
            count += 1
            searchRange = found.upperBound..<haystack.endIndex
        }
        return count
    }

    /// Count non-overlapping matches of `pattern`. `\s` in the pattern
    /// matches newlines, so a call that wraps across lines is counted.
    private static func regexCount(_ pattern: String, in haystack: String) -> Int {
        guard let regex = try? NSRegularExpression(pattern: pattern) else { return 0 }
        let range = NSRange(haystack.startIndex..<haystack.endIndex, in: haystack)
        return regex.numberOfMatches(in: haystack, range: range)
    }

    /// String-literal-aware comment stripper. Removes `//` line comments
    /// and `/* ... */` block comments while preserving `//` that appears
    /// inside a `"..."` string literal (e.g. `"https://..."`), so a
    /// real SQLite URI literal in code is NOT destroyed by naive
    /// `//`-stripping. Not a full Swift lexer (ignores `#"..."#` raw
    /// strings and `'` — neither carries SQLite URIs in this codebase),
    /// but exact for the shapes the guard must distinguish.
    static func strippingComments(_ source: String) -> String {
        enum Mode { case code, string, lineComment, blockComment }
        var mode: Mode = .code
        var out = String()
        out.reserveCapacity(source.count)
        let chars = Array(source)
        var i = 0
        while i < chars.count {
            let c = chars[i]
            let next: Character? = (i + 1 < chars.count) ? chars[i + 1] : nil
            switch mode {
            case .code:
                if c == "/" && next == "/" {
                    mode = .lineComment
                    i += 2
                } else if c == "/" && next == "*" {
                    mode = .blockComment
                    i += 2
                } else if c == "\"" {
                    out.append(c)
                    mode = .string
                    i += 1
                } else {
                    out.append(c)
                    i += 1
                }
            case .string:
                if c == "\\" {
                    // Preserve escape sequences (incl. \( interpolation
                    // open) verbatim — Family 1 keys off `file://\(`.
                    out.append(c)
                    if let n = next {
                        out.append(n)
                        i += 2
                    } else {
                        i += 1
                    }
                } else if c == "\"" {
                    out.append(c)
                    mode = .code
                    i += 1
                } else {
                    out.append(c)
                    i += 1
                }
            case .lineComment:
                if c == "\n" {
                    out.append(c)
                    mode = .code
                }
                i += 1
            case .blockComment:
                if c == "*" && next == "/" {
                    mode = .code
                    i += 2
                } else {
                    // Preserve newlines so line structure is roughly kept.
                    if c == "\n" { out.append(c) }
                    i += 1
                }
            }
        }
        return out
    }
}
