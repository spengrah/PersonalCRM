// CallHistoryOps — body of the CLI ops subcommands:
//   crm-mac call-history backfill --restart [--yes]
//   crm-mac call-history scan --identifier <handle> [--since <duration>]
//
// Both modify the Pi-side phone_calls cursor via the GET -> mutate ->
// POST CAS-commit flow. Mutating local state.json only would be
// silently overwritten on the daemon's next tick when it re-fetches the
// (unchanged) cursor.
//
// Daemon-running guard: both acquire the daemon's pidfile lock before
// any Pi calls. If the daemon is up, the lock acquire throws and we
// surface a user-facing message.
//
// This mirrors MessagesOps' contract on a different source.
import Foundation
import CRMMacCore
import CRMMacPiClient

public enum CallHistoryOpsError: Error, Equatable, CustomStringConvertible {
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

public final class CallHistoryOps: Sendable {
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

    /// Reset the phone_calls cursor to install-time state. Operator is
    /// prompted for "yes" unless `yes: true` is passed.
    public func backfillRestart(yes: Bool) async throws {
        if !yes {
            print("""

            crm-mac call-history backfill --restart will reset the phone_calls
            cursor to install-time state. The daemon's next tick will re-walk
            historical Phone and FaceTime call records back to 2026-01-01 and
            re-emit call.received / call.sent events for every row. The Pi
            dedups on call_unique_id so no duplicate interactions are created;
            re-pushed events will appear in /api/v1/host/:id logs as duplicates.

            """)
            print("Type \"yes\" to confirm: ", terminator: "")
            let response = stdin.readLine() ?? ""
            if response.trimmingCharacters(in: .whitespacesAndNewlines) != "yes" {
                throw CallHistoryOpsError.userDeclined
            }
        }

        try acquireOrThrow()
        defer { pidfileLock.release() }

        let current = try await piClient.getCursor(auth: auth, source: "phone_calls")
        let fresh = PhoneCallsCursorWire(backfillFloorSentAt: backfillFloor)
        let nextJSON = try PhoneCallsCursorWireCodec.encode(fresh)
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: "phone_calls",
                cursor: nextJSON,
                baseCursor: current.cursor,
                cursorEpoch: current.cursorEpoch,
                backfillComplete: false)
        } catch PiClientError.cursorConflict {
            throw CallHistoryOpsError.cursorConflict
        } catch {
            throw CallHistoryOpsError.piUnreachable(
                underlying: String(describing: error))
        }
        logger.info("call-history backfill --restart: cursor reset", metadata: [:])
    }

    /// Queue a targeted backwards scan for `identifier`. The next daemon
    /// tick (operator-initiated start) drains the queue.
    public func scan(identifier: String, since: TimeInterval) async throws {
        let canonical = HandleNormalization.canonicalize(identifier)
        if canonical.isEmpty {
            throw CallHistoryOpsError.invalidIdentifier(identifier)
        }

        try acquireOrThrow()
        defer { pidfileLock.release() }

        let current = try await piClient.getCursor(auth: auth, source: "phone_calls")
        var working = (try? PhoneCallsCursorWireCodec.decode(current.cursor))
            ?? PhoneCallsCursorWire(backfillFloorSentAt: backfillFloor)
        let sinceDate = Date().addingTimeInterval(-since)
        let scan = PhoneCallsCursorPendingScan(normalizedHandle: canonical, since: sinceDate)
        // Bounded queue: drop oldest on overflow.
        if working.pendingScans.count >= PhoneCallsCursorWire.pendingScansCap {
            working.pendingScans.removeFirst()
            logger.warning("call-history scan: pending-scan cap reached; dropped oldest", metadata: [:])
        }
        working.pendingScans.append(scan)
        let nextJSON = try PhoneCallsCursorWireCodec.encode(working)
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: "phone_calls",
                cursor: nextJSON,
                baseCursor: current.cursor,
                cursorEpoch: current.cursorEpoch,
                backfillComplete: working.backfillComplete)
        } catch PiClientError.cursorConflict {
            throw CallHistoryOpsError.cursorConflict
        } catch {
            throw CallHistoryOpsError.piUnreachable(
                underlying: String(describing: error))
        }
        logger.info("call-history scan: pending scan queued", metadata: [
            "handle": .private(canonical),
        ])
    }

    private func acquireOrThrow() throws {
        do {
            try pidfileLock.acquire()
        } catch PidfileError.alreadyHeld(let pid) {
            throw CallHistoryOpsError.daemonRunning(pid: pid)
        }
    }

    /// Helper for HandleNormalization, re-exported so tests don't need
    /// to import CRMMacPhoneCallsSource (this target is Foundation-only
    /// and stays free of GRDB).
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
