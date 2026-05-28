// OrphanNotificationDelegate routes UNUserNotificationCenter
// tap callbacks to OrphanNotificationCenter's click handler.
//
// Apple's API requires an NSObject conforming to the delegate
// protocol (Swift actor types can't be the delegate directly).
// The actor retains this delegate strongly per §D20 — the OS
// holds the delegate `weak`, so without a strong owner the
// delegate would deallocate at the next await boundary and taps
// would silently stop firing.
import Foundation
import UserNotifications

/// NSObject + UNUserNotificationCenterDelegate adapter. Forwards
/// tap callbacks to the owning OrphanNotificationCenter actor.
/// Holds a `weak` reference back to the actor to avoid a retain
/// cycle (actor → delegate → actor); the actor's lifetime spans
/// the daemon process so the weak reference is always valid in
/// practice.
public final class OrphanNotificationDelegate: NSObject, UNUserNotificationCenterDelegate, @unchecked Sendable {
    private weak var center: OrphanNotificationCenter?

    public init(center: OrphanNotificationCenter) {
        self.center = center
        super.init()
    }

    public func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        let userInfo = response.notification.request.content.userInfo
        let reason = userInfo["reason"] as? String
        let sessionUUID = userInfo["session_uuid"] as? String
        let owner = self.center
        // Apple's completionHandler isn't @Sendable in the
        // protocol signature, so we call it on the same dispatch
        // thread immediately and dispatch the actor call
        // independently. Apple's API contract is "call completion
        // when done"; we treat the tap-routing as best-effort
        // (the actor may still be running when completion fires).
        Task { @MainActor in
            await owner?.handleTap(reason: reason, sessionUUID: sessionUUID)
        }
        completionHandler()
    }

    /// Surface notifications even when the daemon is the
    /// foreground app (which is rare — we're a LaunchAgent — but
    /// the system delivers a foreground hint and we want banners
    /// in that case too).
    public func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .sound])
    }
}
