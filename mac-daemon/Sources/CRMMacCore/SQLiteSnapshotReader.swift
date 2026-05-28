import Foundation

/// Owns the canonical read-only SQLite URI shape for every live macOS
/// DB the daemon tails (chat.db, CallHistoryDB, and future WhatsApp).
///
/// The load-bearing invariant is the ABSENCE of `immutable=1`. Live
/// macOS DBs are continuously written in WAL mode and macOS does not
/// checkpoint them on any predictable cadence (CallHistoryDB has been
/// observed stale by days while its -wal sidecar held megabytes of live
/// writes). `immutable=1` tells SQLite to ignore the WAL and serve only
/// the main-file snapshot, so an immutable reader silently misses every
/// uncheckpointed write. `mode=ro` is WAL-aware and is the only flag we
/// need.
///
/// We deliberately do NOT append `cache=shared`: GRDB only honours it
/// for in-memory databases, and for a file-backed read-only
/// DatabasePool (multiple connections) it is a no-op-to-harmful flag.
/// `mode=ro`'s WAL-awareness — not a shared page cache — is what makes
/// uncheckpointed writes visible.
public enum SQLiteSnapshotReader {
    /// Build the read-only, WAL-aware SQLite URI for a live macOS DB at
    /// `path`. Pass the result to `DatabasePool(path:configuration:)`
    /// with `Configuration.readonly = true`.
    ///
    /// `path` is percent-encoded for the URI path component so a path
    /// containing a space, `?`, `#`, or `%` is treated as filename
    /// bytes, not URI syntax (SQLite's `file:` parser splits on the
    /// first `?`). SQLite percent-DEcodes the path component, so the
    /// encoded form round-trips back to the original bytes and opens
    /// the same file. The macOS DBs we tail live under `~/Library/...`
    /// (a path that contains a space, e.g. `Application Support`), and
    /// encoding is also free defense-in-depth for any future path with
    /// URI-significant bytes — the helper is the single chokepoint, so
    /// it is the right place to do it.
    public static func readOnlyURI(for path: String) -> String {
        let encoded = path.addingPercentEncoding(
            withAllowedCharacters: pathAllowed) ?? path
        return "file://\(encoded)?mode=ro"
    }

    /// Convenience overload for a file URL.
    public static func readOnlyURI(for url: URL) -> String {
        readOnlyURI(for: url.path)
    }

    /// `/` must survive (path separators); space, `?`, `#`, `%` must be
    /// encoded. `urlPathAllowed` already excludes space/`?`/`#` and
    /// encodes a raw `%` to `%25`, which is exactly what makes each of
    /// those characters round-trip through SQLite's URI decoder.
    private static let pathAllowed: CharacterSet = {
        var set = CharacterSet.urlPathAllowed
        set.insert("/")
        return set
    }()
}
