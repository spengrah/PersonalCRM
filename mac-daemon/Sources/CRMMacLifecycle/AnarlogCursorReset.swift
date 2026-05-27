// AnarlogCursorReset — the cursor-reset handshake used by
// `crm-mac configure anarlog --reset-cursor`. Lives in
// CRMMacLifecycle (not the executable target) so the conflict-retry
// behavior is unit-testable via a URLProtocol-mocked PiClient.
//
// Contract:
//   1. GET /sync/<source>/cursor to capture the current cursor +
//      epoch.
//   2. POST /sync/<source>/cursor with cursor="", base_cursor =
//      step 1's value, cursor_epoch = step 1's value,
//      backfill_complete = false.
//   3. On 409 conflict: re-GET the cursor (which captures any
//      change that landed between step 1 and step 2) and retry the
//      POST exactly once. A second 409 propagates — the operator
//      should resolve the racing writer out-of-band.
import Foundation
import CRMMacPiClient

public enum AnarlogCursorReset {
    /// Reset a single source's cursor to empty. The retry is
    /// bounded at exactly one re-attempt.
    public static func resetOne(
        client: PiClient,
        auth: PiAuth,
        source: String
    ) async throws {
        let initial = try await client.getCursor(auth: auth, source: source)
        do {
            try await client.commitCursor(
                auth: auth,
                source: source,
                cursor: "",
                baseCursor: initial.cursor,
                cursorEpoch: initial.cursorEpoch,
                backfillComplete: false)
            return
        } catch PiClientError.cursorConflict {
            // fall through to the refetch + retry path below
        }

        let refetched = try await client.getCursor(auth: auth, source: source)
        try await client.commitCursor(
            auth: auth,
            source: source,
            cursor: "",
            baseCursor: refetched.cursor,
            cursorEpoch: refetched.cursorEpoch,
            backfillComplete: false)
    }
}
