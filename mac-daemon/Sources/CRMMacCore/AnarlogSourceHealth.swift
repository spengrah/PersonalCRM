import Foundation

public enum AnarlogCLIFailure: Error, Sendable, Equatable {
    // Permanent class: notify on the tick that observes it.
    case binaryNotFound
    case unsupportedSchemaVersion(String)
    case missingField(command: String, field: String)
    case malformedOutput(command: String)
    case operatorPersonIDUnset
    // Runtime class: notify after two consecutive failed ticks of the same source.
    case databaseNotFound
    case nonZeroExit(code: Int32, errorCode: String?)
    case timeout

    public var isPermanent: Bool {
        switch self {
        case .binaryNotFound,
             .unsupportedSchemaVersion,
             .missingField,
             .malformedOutput,
             .operatorPersonIDUnset:
            true
        case .databaseNotFound,
             .nonZeroExit,
             .timeout:
            false
        }
    }
}

public enum AnarlogTickOutcome: Sendable, Equatable {
    /// The complete listing was read. nil = the listing was empty.
    case clean(newestSessionCreatedAt: Date?)
    /// The complete listing was read, and the tick's deletions exceeded the
    /// deletion cap, so none were sent. withheld = the deletions not sent.
    case deletionsWithheld(withheld: Int, newestSessionCreatedAt: Date?)
    case failed(AnarlogCLIFailure)
}

public protocol AnarlogHealthSink: Sendable {
    func record(source: SourceID, outcome: AnarlogTickOutcome) async
}

public struct NoopAnarlogHealthSink: AnarlogHealthSink {
    public init() {}

    public func record(source: SourceID, outcome: AnarlogTickOutcome) async {}
}
