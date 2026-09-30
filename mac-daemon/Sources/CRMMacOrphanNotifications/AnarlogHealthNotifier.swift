// AnarlogHealthNotifier — S5 source-health sink. Its anarlog_source:
// identifiers sit outside OrphanNotificationCenter's orphan/conflict sweep.
import Foundation
import CRMMacCore

enum AnarlogBrokenCondition: String, CaseIterable, Sendable {
    case binaryNotFound = "binary_not_found"
    case unsupportedSchemaVersion = "unsupported_schema_version"
    case missingField = "missing_field"
    case malformedOutput = "malformed_output"
    case operatorPersonIDUnset = "operator_person_id_unset"
    case databaseNotFound = "database_not_found"
    case nonZeroExit = "nonzero_exit"
    case timeout = "timeout"
    case deletionsWithheld = "deletions_withheld"

    var notificationIdentifier: String {
        "anarlog_source:broken:" + rawValue
    }

    init(_ failure: AnarlogCLIFailure) {
        switch failure {
        case .binaryNotFound:
            self = .binaryNotFound
        case .unsupportedSchemaVersion:
            self = .unsupportedSchemaVersion
        case .missingField:
            self = .missingField
        case .malformedOutput:
            self = .malformedOutput
        case .operatorPersonIDUnset:
            self = .operatorPersonIDUnset
        case .databaseNotFound:
            self = .databaseNotFound
        case .nonZeroExit:
            self = .nonZeroExit
        case .timeout:
            self = .timeout
        }
    }
}

enum AnarlogHealthNotification: Hashable, Sendable {
    case broken(AnarlogBrokenCondition)
    case quiet

    static let all: [AnarlogHealthNotification] =
        AnarlogBrokenCondition.allCases.map { .broken($0) } + [.quiet]

    var identifier: String {
        switch self {
        case .broken(let condition):
            return condition.notificationIdentifier
        case .quiet:
            return AnarlogHealthNotifier.quietIdentifier
        }
    }
}

public actor AnarlogHealthNotifier: AnarlogHealthSink {
    public static let quietIdentifier = "anarlog_source:quiet"
    public static let quietWindow: TimeInterval = 14 * 24 * 60 * 60
    public static let runtimeFailureThreshold = 2

    private enum Presence {
        case unknown
        case shown
        case absent
    }

    private struct HeldCondition {
        let condition: AnarlogBrokenCondition
        let request: NotificationRequestSpec
        let reportOrder: UInt64
    }

    private struct SourceState {
        var runtimeFailureStreak = 0
        var heldCondition: HeldCondition?
    }

    private struct NotificationState {
        var presence: Presence = .unknown
        var shouldShow: Bool?
        var desiredRequest: NotificationRequestSpec?
    }

    private let presenter: UserNotificationPresenter
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date
    private var sourceStates: [SourceID: SourceState] = [:]
    private var quietHolds: Bool?
    private var notificationStates: [AnarlogHealthNotification: NotificationState]
    private var reportOrder: UInt64 = 0
    private var reconciling = false
    private var dirty = false

    public init(
        presenter: UserNotificationPresenter,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date
    ) {
        self.presenter = presenter
        self.logger = logger
        self.clock = clock
        self.notificationStates = Dictionary(
            uniqueKeysWithValues: AnarlogHealthNotification.all.map { ($0, NotificationState()) })
    }

    public func record(source: SourceID, outcome: AnarlogTickOutcome) async {
        var state = sourceStates[source] ?? SourceState()
        switch outcome {
        case .clean(let newestSessionCreatedAt):
            state.runtimeFailureStreak = 0
            state.heldCondition = nil
            quietHolds = isQuiet(newestSessionCreatedAt)
        case .deletionsWithheld(let withheld, let newestSessionCreatedAt):
            state.runtimeFailureStreak = 0
            state.heldCondition = HeldCondition(
                condition: .deletionsWithheld,
                request: notificationSpec(
                    identifier: AnarlogBrokenCondition.deletionsWithheld.notificationIdentifier,
                    title: "Anarlog sync is broken",
                    body: "\(withheld) Anarlog session deletions were withheld because they exceed the deletion cap. To apply them, raise it with: crm-mac configure anarlog --deletion-cap"),
                reportOrder: nextReportOrder())
            quietHolds = isQuiet(newestSessionCreatedAt)
        case .failed(let failure):
            let condition = AnarlogBrokenCondition(failure)
            if failure.isPermanent {
                state.runtimeFailureStreak = 0
                state.heldCondition = HeldCondition(
                    condition: condition,
                    request: spec(for: failure, condition: condition),
                    reportOrder: nextReportOrder())
            } else {
                state.runtimeFailureStreak += 1
                state.heldCondition = state.runtimeFailureStreak >= Self.runtimeFailureThreshold
                    ? HeldCondition(
                        condition: condition,
                        request: spec(for: failure, condition: condition),
                        reportOrder: nextReportOrder())
                    : nil
            }
        }
        sourceStates[source] = state
        refreshDesiredRequests()

        dirty = true
        if reconciling {
            return
        }

        reconciling = true
        while dirty {
            dirty = false
            await reconcilePass()
        }
        reconciling = false
    }

    private func nextReportOrder() -> UInt64 {
        reportOrder += 1
        return reportOrder
    }

    private func refreshDesiredRequests() {
        for notification in AnarlogHealthNotification.all {
            let shouldShow: Bool?
            let request: NotificationRequestSpec?
            switch notification {
            case .broken(let condition):
                let heldCondition = sourceStates.values
                    .compactMap(\.heldCondition)
                    .filter { $0.condition == condition }
                    .max { $0.reportOrder < $1.reportOrder }
                shouldShow = heldCondition.map { _ in true } ?? false
                request = heldCondition?.request
            case .quiet:
                shouldShow = quietHolds
                if quietHolds == true {
                    request = notificationSpec(
                        identifier: Self.quietIdentifier,
                        title: "No new Anarlog sessions",
                        body: "Anarlog lists no session created in the last 14 days.")
                } else {
                    request = nil
                }
            }
            notificationStates[notification]?.shouldShow = shouldShow
            notificationStates[notification]?.desiredRequest = request
        }
    }

    private func isQuiet(_ newestSessionCreatedAt: Date?) -> Bool {
        guard let newestSessionCreatedAt else { return true }
        return clock().timeIntervalSince(newestSessionCreatedAt) > Self.quietWindow
    }

    private func spec(for failure: AnarlogCLIFailure, condition: AnarlogBrokenCondition) -> NotificationRequestSpec {
        let body: String
        switch failure {
        case .binaryNotFound:
            body = "The anarlog CLI was not found. Set its path with: crm-mac configure anarlog --cli-path"
        case .unsupportedSchemaVersion(let version):
            body = "The anarlog CLI reports contract version \"\(version)\"; crm-mac supports \"1\"."
        case .missingField(let command, let field):
            body = "The anarlog CLI output for \(command) lacks the field \(field)."
        case .malformedOutput(let command):
            body = "The anarlog CLI returned unreadable output for \(command)."
        case .operatorPersonIDUnset:
            body = "Your Anarlog person ID is not set. Set it with: crm-mac configure anarlog --operator-person-id"
        case .databaseNotFound:
            body = "The anarlog CLI cannot find the Anarlog database."
        case .nonZeroExit(let code, let errorCode):
            if let errorCode {
                body = "The anarlog CLI exited with code \(code) (\(errorCode))."
            } else {
                body = "The anarlog CLI exited with code \(code)."
            }
        case .timeout:
            body = "The anarlog CLI did not answer in time."
        }
        return notificationSpec(
            identifier: condition.notificationIdentifier,
            title: "Anarlog sync is broken",
            body: body)
    }

    private func notificationSpec(identifier: String, title: String, body: String) -> NotificationRequestSpec {
        NotificationRequestSpec(
            identifier: identifier,
            title: title,
            body: body,
            userInfo: [:],
            sound: true)
    }

    private func reconcilePass() async {
        let notificationsToRemove = AnarlogHealthNotification.all.filter { notification in
            guard let state = notificationStates[notification] else { return false }
            return state.shouldShow == false && state.presence != .absent
        }
        if !notificationsToRemove.isEmpty {
            for notification in notificationsToRemove {
                notificationStates[notification]?.presence = .absent
            }
            let identifiers = notificationsToRemove.map(\.identifier)
            await presenter.removeDelivered(withIdentifiers: identifiers)
            await presenter.removePending(withIdentifiers: identifiers)
        }

        let notificationsToAdd = AnarlogHealthNotification.all.filter { notification in
            guard let state = notificationStates[notification] else { return false }
            return state.shouldShow == true && state.presence != .shown
        }
        for notification in notificationsToAdd {
            let authorized = await presenter.requestAuthorization()
            guard let currentState = notificationStates[notification],
                  currentState.shouldShow == true,
                  let request = currentState.desiredRequest else {
                continue
            }
            guard authorized else {
                logger.warning("anarlog-health: notification authorization denied for \(notification.identifier)")
                notificationStates[notification]?.presence = .absent
                continue
            }

            do {
                try await presenter.add(request)
                notificationStates[notification]?.presence = .shown
            } catch {
                logger.warning(
                    "anarlog-health: presenter.add threw; notification remains absent for retry",
                    metadata: [
                        "identifier": .public(notification.identifier),
                        "error": .private(String(describing: error)),
                    ])
                notificationStates[notification]?.presence = .absent
            }
        }
    }
}
