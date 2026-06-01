// MessagesOps — body of the CLI ops subcommands:
//   crm-mac messages backfill --restart [--yes]
//   crm-mac messages scan --identifier <handle> [--since <duration>]
//
// Both commands modify the Pi-side cursor via the GET -> mutate -> POST
// CAS-commit flow.  Mutating local
// state.json only would be silently overwritten on the daemon's next
// tick when it re-fetches the (unchanged) cursor.
//
// Daemon-running guard: both subcommands acquire the daemon's pidfile
// lock before any Pi calls.  If the daemon is up, the lock acquire
// throws and we surface a user-facing message.
import Foundation
import CRMMacCore
import CRMMacPiClient

public enum MessagesOpsError: Error, Equatable, CustomStringConvertible {
    case daemonRunning(pid: pid_t)
    case piUnreachable(underlying: String)
    case cursorConflict
    case userDeclined
    case invalidIdentifier(String)
    case opDescription(String)

    public var description: String {
        switch self {
        case .daemonRunning(let pid):
            return "daemon is running (PID \(pid)). Stop it before running this command."
        case .piUnreachable(let u):
            return "Pi unreachable: \(u). Retry later."
        case .cursorConflict:
            return "cursor conflict: the daemon (or another ops) raced with this command. Retry."
        case .userDeclined:
            return "operator declined; no changes made."
        case .invalidIdentifier(let s):
            return "invalid identifier: \(s)"
        case .opDescription(let s):
            return s
        }
    }
}

/// Read a single line of input.  Used by `backfill --restart` to
/// prompt for confirmation.  Stubbed in tests.
public protocol MessagesOpsStdinReader: Sendable {
    func readLine() -> String?
}

public struct SystemStdinReader: MessagesOpsStdinReader {
    public init() {}
    public func readLine() -> String? {
        Swift.readLine(strippingNewline: true)
    }
}

public final class MessagesOps: Sendable {
    private let piClient: PiClient
    private let auth: PiAuth
    private let pidfileLock: PidfileLock
    private let logger: LoggerProtocol
    private let stdin: MessagesOpsStdinReader
    private let backfillFloor: Date

    public init(
        piClient: PiClient,
        auth: PiAuth,
        pidfileLock: PidfileLock,
        logger: LoggerProtocol,
        backfillFloor: Date,
        stdin: MessagesOpsStdinReader = SystemStdinReader()
    ) {
        self.piClient = piClient
        self.auth = auth
        self.pidfileLock = pidfileLock
        self.logger = logger
        self.stdin = stdin
        self.backfillFloor = backfillFloor
    }

    /// Reset the messages cursor to install-time state.  Operator is
    /// prompted for "yes" unless `yes: true` is passed.
    public func backfillRestart(yes: Bool) async throws {
        if !yes {
            print("""

            crm-mac messages backfill --restart will reset the messages cursor
            to install-time state. This will re-walk historical messages back to
            2026-01-01, which can take days for large mailboxes and will produce
            duplicate-but-deduplicated events on the Pi (no contact-side effect,
            but visible in /api/v1/host/:id logs).

            """)
            print("Type \"yes\" to confirm: ", terminator: "")
            let response = stdin.readLine() ?? ""
            if response.trimmingCharacters(in: .whitespacesAndNewlines) != "yes" {
                throw MessagesOpsError.userDeclined
            }
        }

        try acquireOrThrow()
        defer { pidfileLock.release() }

        // GET cursor -> compute fresh cursor -> CAS commit.
        let current = try await piClient.getCursor(auth: auth, source: "messages")
        let fresh = MessagesCursor(backfillFloorSentAt: backfillFloor)
        let nextJSON = try MessagesCursorCodec.encode(fresh)
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: "messages",
                cursor: nextJSON,
                baseCursor: current.cursor,
                cursorEpoch: current.cursorEpoch,
                backfillComplete: false)
        } catch PiClientError.cursorConflict {
            throw MessagesOpsError.cursorConflict
        } catch {
            throw MessagesOpsError.piUnreachable(
                underlying: String(describing: error))
        }
        logger.info("messages backfill --restart: cursor reset", metadata: [:])
    }

    /// Queue a targeted backwards scan for `identifier`.  The next
    /// daemon tick (operator-initiated start) drains the queue.
    public func scan(identifier: String, since: TimeInterval) async throws {
        let canonical = HandleNormalization.canonicalize(identifier)
        if canonical.isEmpty {
            throw MessagesOpsError.invalidIdentifier(identifier)
        }

        try acquireOrThrow()
        defer { pidfileLock.release() }

        let current = try await piClient.getCursor(auth: auth, source: "messages")
        var working = (try? MessagesCursorCodec.decode(current.cursor))
            ?? MessagesCursor(backfillFloorSentAt: backfillFloor)
        // Never scan below the backfill floor — rows older than it are
        // never emitted, so a wider `--since` would just waste work.
        let sinceDate = max(Date().addingTimeInterval(-since), backfillFloor)
        // Coverage-dedup: at most one entry per handle, keeping the WIDER
        // window. A wider window resets progress so the larger range is
        // re-walked; an equal-or-narrower window leaves the existing
        // entry untouched. Mirrors the source tick's merge.
        mergePendingScan(into: &working.pendingScans, handle: canonical, since: sinceDate)
        let nextJSON = try MessagesCursorCodec.encode(working)
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: "messages",
                cursor: nextJSON,
                baseCursor: current.cursor,
                cursorEpoch: current.cursorEpoch,
                backfillComplete: working.backfillComplete)
        } catch PiClientError.cursorConflict {
            throw MessagesOpsError.cursorConflict
        } catch {
            throw MessagesOpsError.piUnreachable(
                underlying: String(describing: error))
        }
        logger.info("messages scan: pending scan queued", metadata: [
            "handle": .private(canonical),
        ])
    }

    /// Merge identifier `handle` into a pendingScans list with
    /// COVERAGE-DEDUP (at most one entry per handle, keeping the wider
    /// window) + cap (drop oldest on overflow). Keeps the operator path
    /// idempotent and consistent with the source tick's merge.
    private func mergePendingScan(
        into scans: inout [PendingScan],
        handle: String,
        since: Date
    ) {
        if let idx = scans.firstIndex(where: { $0.normalizedHandle == handle }) {
            if since < scans[idx].since {
                scans[idx] = PendingScan(normalizedHandle: handle, since: since)
            }
            return
        }
        scans.append(PendingScan(normalizedHandle: handle, since: since))
        if scans.count > MessagesCursor.pendingScansCap {
            scans.removeFirst(scans.count - MessagesCursor.pendingScansCap)
            logger.warning("messages scan: pending-scan cap reached; dropped oldest", metadata: [:])
        }
    }

    private func acquireOrThrow() throws {
        do {
            try pidfileLock.acquire()
        } catch PidfileError.alreadyHeld(let pid) {
            throw MessagesOpsError.daemonRunning(pid: pid)
        }
    }

    /// Helper for HandleNormalization, re-exported so tests don't
    /// need to import CRMMacMessagesSource (this target is
    /// Foundation-only and stays free of GRDB).
    ///
    /// Returns the canonical form, mirroring
    /// CRMMacMessagesSource.HandleNormalization.canonicalize. Email
    /// shape (`@`) -> lowercase + trim; otherwise treat as phone +
    /// normalize to E.164 (NANP for 10-digit).
    private enum HandleNormalization {
        static func canonicalize(_ raw: String) -> String {
            let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
            if trimmed.isEmpty { return "" }
            if trimmed.contains("@") {
                return NormalizationParity.normalizeEmail(trimmed)
            }
            return NormalizationParity.normalizePhoneE164(trimmed)
        }
    }
}

/// Cursor wire types are shared between MessagesOps and the source
/// plugin via CRMMacCore.MessagesCursorWire. Aliasing here keeps the
/// file-internal call sites concise while making the cross-target
/// shared schema explicit at the type-system level.
typealias MessagesCursor = MessagesCursorWire
typealias PendingScan = MessagesCursorPendingScan
typealias MessagesCursorCodec = MessagesCursorWireCodec
