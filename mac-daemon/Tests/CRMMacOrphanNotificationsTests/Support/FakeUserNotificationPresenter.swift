// FakeUserNotificationPresenter — recording fake for tests.
// Records every method call in order so assertions can verify
// the actor produced the expected OS-side requests.
import Foundation
import UserNotifications
@testable import CRMMacOrphanNotifications

/// Local alias for the production wrapper, used to keep the existing
/// test assertions terse.
public typealias DelegateRef = UserNotificationDelegateRef

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
    private var seededDelivered: [String] = []
    private var seededPending: [String] = []
    // Gate used by the reentrancy test: when set, add(_:) parks
    // the caller on this continuation until releaseAddGate() is
    // called. Lets the test deterministically hold the first
    // raise mid-flight (after the pre-add persist, before the
    // confirmPostAdd write) and then run additional consume()
    // calls to verify they hit the raisesInFlight guard.
    private var addGate: CheckedContinuation<Void, Never>?
    private var addGateArmed: Bool = false
    private var addsAwaitingGate: Int = 0

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

    /// Seed the identifiers `getDeliveredIdentifiers()` will return —
    /// used by the legacy-notification-sweep test to simulate
    /// pre-versioned ids already in Notification Center.
    public func seedDeliveredIdentifiers(_ ids: [String]) {
        seededDelivered = ids
    }

    /// Seed the identifiers `getPendingIdentifiers()` will return.
    public func seedPendingIdentifiers(_ ids: [String]) {
        seededPending = ids
    }

    /// Arm the add-gate. While armed, the next add(_:) call will
    /// suspend on a continuation; releaseAddGate() resumes it.
    /// Used by tests to deterministically hold a raise mid-flight.
    public func armAddGate() {
        addGateArmed = true
    }

    /// Release a single waiter parked on the add-gate (if any) and
    /// disarm the gate so subsequent add(_:) calls return
    /// immediately. Idempotent and safe to call even if no waiter
    /// is parked.
    public func releaseAddGate() {
        addGateArmed = false
        if let cont = addGate {
            addGate = nil
            cont.resume()
        }
    }

    /// Number of add(_:) calls currently parked on the gate. Tests
    /// poll this to confirm the first raise is mid-flight before
    /// kicking off the second.
    public func addsCurrentlyAwaitingGate() -> Int {
        addsAwaitingGate
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
        // Gate only the FIRST add(_:) call after armAddGate(); any
        // subsequent call returns immediately. This keeps the
        // reentrancy test deterministic: if raisesInFlight regresses
        // and a second add(_:) lands while the first is parked, it
        // won't get stuck waiting on the same single-slot
        // continuation — instead it returns, the test sees a second
        // recorded call, and fails cleanly with the expected
        // assertion mismatch.
        if addGateArmed && addGate == nil {
            addGateArmed = false
            addsAwaitingGate += 1
            await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
                addGate = cont
            }
            addsAwaitingGate -= 1
        }
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

    public func setDelegate(_ ref: UserNotificationDelegateRef?) async {
        setDelegateCalls += 1
        lastDelegate = ref
    }

    public func getDeliveredIdentifiers() async -> [String] {
        seededDelivered
    }

    public func getPendingIdentifiers() async -> [String] {
        seededPending
    }
}
