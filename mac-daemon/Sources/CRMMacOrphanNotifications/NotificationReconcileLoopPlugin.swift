// NotificationReconcileLoopPlugin — a SourcePlugin wrapper that
// calls OrphanNotificationCenter.reconcile() every 300s.
//
// The startup reconcile happens explicitly via DaemonCommand
// wiring a FirstSuccessLatch onto HeartbeatLoop; this loop is
// the steady-state safety net for "user resolved a conflict in
// the Pi UI but the Mac notification still shows".
//
// Failures inside reconcile() are logged but never propagated —
// this is a UX nice-to-have, not a correctness path.
import Foundation
import CRMMacCore

public final class NotificationReconcileLoopPlugin: SourcePlugin, @unchecked Sendable {
    public let id: SourceID = .notificationReconcile
    public let tickInterval: TimeInterval

    private let center: OrphanNotificationCenter
    private let logger: LoggerProtocol

    public init(
        center: OrphanNotificationCenter,
        logger: LoggerProtocol,
        tickInterval: TimeInterval = 300
    ) {
        self.center = center
        self.logger = logger
        self.tickInterval = tickInterval
    }

    public func tick() async throws {
        // reconcile() never throws — it logs internally and
        // returns. Wrap defensively anyway so any future
        // behavior change doesn't escalate as a tick failure.
        await center.reconcile()
        logger.debug("notification_reconcile: tick complete")
    }
}
