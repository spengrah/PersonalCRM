import Foundation
import CRMMacCore

public struct AnarlogConfigureRequest: Equatable, Sendable {
    public var operatorPersonID: String?
    public var cliPath: String?
    public var deletionCap: Int?

    public init(operatorPersonID: String?, cliPath: String?, deletionCap: Int?) {
        self.operatorPersonID = operatorPersonID
        self.cliPath = cliPath
        self.deletionCap = deletionCap
    }
}

public enum AnarlogConfigureFlow {
    public static func apply(
        _ request: AnarlogConfigureRequest,
        to config: inout AnarlogConfig
    ) throws(AnarlogConfigError) {
        var updated = config
        if let operatorPersonID = request.operatorPersonID {
            try updated.setOperatorPersonID(operatorPersonID)
        }
        if let cliPath = request.cliPath {
            try updated.setCLIPath(cliPath)
        }
        if let deletionCap = request.deletionCap {
            try updated.setDeletionCap(deletionCap)
        }
        config = updated
    }

    public static func message(for error: AnarlogConfigError) -> String {
        switch error {
        case .operatorPersonIDNotUUID(let raw):
            return "anarlog operator person id must be a UUID, got: \(raw)"
        case .cliPathNotAbsolute(let raw):
            return "anarlog cli path must be absolute, got: \(raw)"
        case .negativeDeletionCap(let value):
            return "anarlog deletion cap must be non-negative, got: \(value)"
        }
    }

    public static func summaryLines(_ config: AnarlogConfig) -> [String] {
        [
            "  operator_person_id: \(config.operatorPersonID ?? "(unset)")",
            "  cli_path:           \(config.cliPath ?? "(default search)")",
            "  deletion_cap:       \(config.deletionCap)",
        ]
    }
}
