// FakeUserNotificationPresenter — recording fake for tests.
// Records every method call in order so assertions can verify
// the actor produced the expected OS-side requests.
import Foundation
import UserNotifications
@testable import CRMMacOrphanNotifications

/// Sendable wrapper around UNUserNotificationCenterDelegate so the
/// fake actor can store the reference without violating Swift 6's
/// strict-concurrency check. The delegate type itself is an
/// `@unchecked Sendable` NSObject in the production OrphanNotificationDelegate.
public struct DelegateRef: @unchecked Sendable {
    public let value: UNUserNotificationCenterDelegate
}

/// Records calls to UserNotificationPresenter. The
/// `authorizationResult` constructor argument seeds the first
/// requestAuthorization() result; subsequent calls can be
/// re-seeded via `setAuthorizationResult(_:)`. Optional
/// `addError` simulates a transient OS failure on add(_:).
public actor FakeUserNotificationPresenter: UserNotificationPresenter {
    private var authorizationResult: Bool
    private var addError: Error?
    public private(set) var addCalls: [NotificationRequestSpec] = []
    public private(set) var removeDeliveredCalls: [[String]] = []
    public private(set) var removePendingCalls: [[String]] = []
    public private(set) var setDelegateCalls: Int = 0
    public private(set) var requestAuthorizationCalls: Int = 0
    private var lastDelegate: DelegateRef?

    public init(authorizationResult: Bool = true, addError: Error? = nil) {
        self.authorizationResult = authorizationResult
        self.addError = addError
    }

    public func setAuthorizationResult(_ value: Bool) {
        self.authorizationResult = value
    }

    public func setAddError(_ value: Error?) {
        self.addError = value
    }

    /// Test accessor — returns the recorded list of add() specs.
    public func recordedAddCalls() -> [NotificationRequestSpec] { addCalls }

    public func recordedRemoveDelivered() -> [[String]] { removeDeliveredCalls }
    public func recordedRemovePending() -> [[String]] { removePendingCalls }
    public func recordedSetDelegateCount() -> Int { setDelegateCalls }
    public func recordedRequestAuthorizationCount() -> Int { requestAuthorizationCalls }
    public func currentDelegate() -> DelegateRef? { lastDelegate }

    // MARK: - UserNotificationPresenter

    public func requestAuthorization() async -> Bool {
        requestAuthorizationCalls += 1
        return authorizationResult
    }

    public func add(_ spec: NotificationRequestSpec) async throws {
        addCalls.append(spec)
        if let err = addError {
            throw err
        }
    }

    public func removeDelivered(withIdentifiers ids: [String]) async {
        removeDeliveredCalls.append(ids)
    }

    public func removePending(withIdentifiers ids: [String]) async {
        removePendingCalls.append(ids)
    }

    public func setDelegate(_ delegate: sending UNUserNotificationCenterDelegate?) async {
        setDelegateCalls += 1
        if let d = delegate {
            lastDelegate = DelegateRef(value: d)
        } else {
            lastDelegate = nil
        }
    }
}
