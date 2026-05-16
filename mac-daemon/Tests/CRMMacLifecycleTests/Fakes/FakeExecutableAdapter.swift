import Foundation
@testable import CRMMacLifecycle

public final class FakeExecutableAdapter: ExecutableAdapter, @unchecked Sendable {
    public private(set) var codesignCalls: [String] = []
    public var pathToReport: String?
    public var failCodesignWith: String?

    public init(currentExecutablePath: String = "/tmp/source/crm-mac") {
        self.pathToReport = currentExecutablePath
    }

    public func currentExecutablePath() throws -> String {
        guard let path = pathToReport else {
            throw ExecutableAdapterError.notFound
        }
        return path
    }

    public func adhocCodesign(path: String) throws {
        codesignCalls.append(path)
        if let reason = failCodesignWith {
            throw ExecutableAdapterError.codesignFailed(reason)
        }
    }
}
