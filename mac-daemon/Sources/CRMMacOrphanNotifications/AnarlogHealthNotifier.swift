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

public actor AnarlogHealthNotifier: AnarlogHealthSink {
    public static let quietIdentifier = "anarlog_source:quiet"
    public static let quietWindow: TimeInterval = 14 * 24 * 60 * 60
    public static let runtimeFailureThreshold = 2

    private enum Presence {
        case unknown
        case shown
        case absent
    }

    private struct SourceState {
        var runtimeFailureStreak = 0
        var heldCondition: AnarlogBrokenCondition?
    }

    private let presenter: UserNotificationPresenter
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date
    private var sourceStates: [SourceID: SourceState] = [:]
    private var latestSpecs: [String: NotificationRequestSpec] = [:]
    private var quietHolds: Bool?
    private var presence: [String: Presence]
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
        self.presence = Dictionary(
            uniqueKeysWithValues: Self.orderedIdentifiers.map { ($0, .unknown) })
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
            state.heldCondition = .deletionsWithheld
            quietHolds = isQuiet(newestSessionCreatedAt)
            latestSpecs[AnarlogBrokenCondition.deletionsWithheld.notificationIdentifier] =
                notificationSpec(
                    identifier: AnarlogBrokenCondition.deletionsWithheld.notificationIdentifier,
                    title: "Anarlog sync is broken",
                    body: "\(withheld) Anarlog session deletions were withheld because they exceed the deletion cap. To apply them, raise it with: crm-mac configure anarlog --deletion-cap")
        case .failed(let failure):
            let condition = AnarlogBrokenCondition(failure)
            if failure.isPermanent {
                state.runtimeFailureStreak = 0
                state.heldCondition = condition
            } else {
                state.runtimeFailureStreak += 1
                state.heldCondition = state.runtimeFailureStreak >= Self.runtimeFailureThreshold
                    ? condition
                    : nil
            }
            if state.heldCondition != nil {
                latestSpecs[condition.notificationIdentifier] = spec(for: failure, condition: condition)
            }
        }
        sourceStates[source] = state

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

    private static var orderedIdentifiers: [String] {
        AnarlogBrokenCondition.allCases.map(\.notificationIdentifier) + [quietIdentifier]
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

    private func desiredState(for identifier: String) -> Bool? {
        if identifier == Self.quietIdentifier {
            return quietHolds
        }
        guard let condition = AnarlogBrokenCondition.allCases.first(where: {
            $0.notificationIdentifier == identifier
        }) else {
            return false
        }
        return sourceStates.values.contains { $0.heldCondition == condition }
    }

    private func spec(for identifier: String) -> NotificationRequestSpec {
        if identifier == Self.quietIdentifier {
            return notificationSpec(
                identifier: Self.quietIdentifier,
                title: "No new Anarlog sessions",
                body: "Anarlog lists no session created in the last 14 days.")
        }
        return latestSpecs[identifier]!
    }

    private func reconcilePass() async {
        let identifiersToRemove = Self.orderedIdentifiers.filter { identifier in
            desiredState(for: identifier) == false && presence[identifier] != .absent
        }
        if !identifiersToRemove.isEmpty {
            for identifier in identifiersToRemove {
                presence[identifier] = .absent
            }
            await presenter.removeDelivered(withIdentifiers: identifiersToRemove)
            await presenter.removePending(withIdentifiers: identifiersToRemove)
        }

        let identifiersToAdd = Self.orderedIdentifiers.filter { identifier in
            desiredState(for: identifier) == true && presence[identifier] != .shown
        }
        for identifier in identifiersToAdd {
            guard await presenter.requestAuthorization() else {
                logger.warning("anarlog-health: notification authorization denied for \(identifier)")
                presence[identifier] = .absent
                continue
            }

            let request = spec(for: identifier)
            do {
                try await presenter.add(request)
                presence[identifier] = .shown
            } catch {
                logger.warning("anarlog-health: notification delivery failed for \(identifier)")
                presence[identifier] = .absent
            }
        }
    }
}
