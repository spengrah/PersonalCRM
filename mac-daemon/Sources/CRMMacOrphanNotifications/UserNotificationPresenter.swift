// UserNotificationPresenter — thin façade over
// UNUserNotificationCenter so tests can inject a fake without
// touching real OS notification state.
//
// Production impl wraps UNUserNotificationCenter.current(); tests
// inject a recording fake actor.
//
// The protocol takes a Sendable NotificationRequestSpec rather
// than UNNotificationRequest directly because the OS class isn't
// Sendable and Swift 6 strict-concurrency forbids sending it
// across actor boundaries. The presenter constructs the OS
// request internally from the spec.
import Foundation
import UserNotifications

/// Sendable wrapper around UNUserNotificationCenterDelegate.
/// Swift 6 strict concurrency rejects passing a non-Sendable class
/// reference (the delegate) across actor isolation; the actual
/// delegate object is `@unchecked Sendable` (NSObject subclass with
/// no mutable per-instance state), so we transport it through this
/// trivial wrapper. The `sending` keyword on a parameter does not
/// suffice on Swift 6.0 protocol-witness implementations under
/// actor isolation, so we bake the Sendable contract into the
/// argument type instead.
public struct UserNotificationDelegateRef: @unchecked Sendable {
    public let value: UNUserNotificationCenterDelegate
    public init(_ value: UNUserNotificationCenterDelegate) {
        self.value = value
    }
}

/// Sendable wire-shape for a notification request. The presenter
/// converts this to UNNotificationRequest internally; the fake
/// records the spec for test assertions.
public struct NotificationRequestSpec: Sendable, Equatable {
    public let identifier: String
    public let title: String
    public let body: String
    public let userInfo: [String: String]
    public let sound: Bool

    public init(
        identifier: String,
        title: String,
        body: String,
        userInfo: [String: String],
        sound: Bool = true
    ) {
        self.identifier = identifier
        self.title = title
        self.body = body
        self.userInfo = userInfo
        self.sound = sound
    }
}

/// Async, Sendable façade over UNUserNotificationCenter. Production
/// impl bridges to the singleton; tests inject a recording fake.
public protocol UserNotificationPresenter: Sendable {
    /// Request notification authorization (`.alert + .sound`).
    /// Returns true when granted, false on denial OR any underlying
    /// error (we treat the two equivalently — the next consume() /
    /// reconcile() will retry, so a transient OS hiccup self-heals).
    func requestAuthorization() async -> Bool

    /// Add a notification request to the OS pending queue. Throws
    /// on any underlying error; the caller records "failed" and
    /// retries on the next opportunity.
    func add(_ spec: NotificationRequestSpec) async throws

    /// Remove already-delivered notifications with the given
    /// identifiers from Notification Center.
    func removeDelivered(withIdentifiers ids: [String]) async

    /// Remove pending (not-yet-delivered) requests with the given
    /// identifiers from the OS queue.
    func removePending(withIdentifiers ids: [String]) async

    /// Register a delegate for tap-handling. The OS holds the
    /// delegate `weak`, so the caller MUST retain it independently
    /// or taps stop firing. Wrapped in `UserNotificationDelegateRef`
    /// (Sendable) because Swift 6 strict concurrency forbids passing
    /// a raw non-Sendable class reference into an actor-isolated
    /// implementation.
    func setDelegate(_ ref: UserNotificationDelegateRef?) async

    /// Identifiers of notifications the OS has already delivered to
    /// Notification Center. Returns identifier strings only (the
    /// caller never needs the full UNNotification, which isn't
    /// Sendable). Used by the startup migration sweep to find
    /// ghost notifications minted by older daemon builds.
    func getDeliveredIdentifiers() async -> [String]

    /// Identifiers of notifications enqueued with the OS but not yet
    /// delivered. Same shape as `getDeliveredIdentifiers()` for the
    /// pending queue.
    func getPendingIdentifiers() async -> [String]
}

/// Production conformance — wraps `UNUserNotificationCenter.current()`.
/// Marked `@unchecked Sendable` because Apple's center is documented
/// thread-safe but lacks formal Sendable conformance.
public final class UserNotificationCenterPresenter: UserNotificationPresenter, @unchecked Sendable {
    public init() {}

    public func requestAuthorization() async -> Bool {
        let center = UNUserNotificationCenter.current()
        do {
            return try await center.requestAuthorization(options: [.alert, .sound])
        } catch {
            return false
        }
    }

    public func add(_ spec: NotificationRequestSpec) async throws {
        let content = UNMutableNotificationContent()
        content.title = spec.title
        content.body = spec.body
        if spec.sound {
            content.sound = .default
        }
        // UNMutableNotificationContent.userInfo expects
        // [AnyHashable: Any]; we keep our spec strict-typed.
        content.userInfo = spec.userInfo
        let request = UNNotificationRequest(
            identifier: spec.identifier,
            content: content,
            trigger: nil)
        let center = UNUserNotificationCenter.current()
        try await center.add(request)
    }

    public func removeDelivered(withIdentifiers ids: [String]) async {
        let center = UNUserNotificationCenter.current()
        center.removeDeliveredNotifications(withIdentifiers: ids)
    }

    public func removePending(withIdentifiers ids: [String]) async {
        let center = UNUserNotificationCenter.current()
        center.removePendingNotificationRequests(withIdentifiers: ids)
    }

    public func setDelegate(_ ref: UserNotificationDelegateRef?) async {
        let center = UNUserNotificationCenter.current()
        center.delegate = ref?.value
    }

    public func getDeliveredIdentifiers() async -> [String] {
        let center = UNUserNotificationCenter.current()
        let delivered = await center.deliveredNotifications()
        return delivered.map { $0.request.identifier }
    }

    public func getPendingIdentifiers() async -> [String] {
        let center = UNUserNotificationCenter.current()
        let pending = await center.pendingNotificationRequests()
        return pending.map { $0.identifier }
    }
}
