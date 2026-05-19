import Foundation
@testable import CRMMacLifecycle

public final class FakeExecutableAdapter: ExecutableAdapter, @unchecked Sendable {
    public struct BundleCodesignCall: Equatable {
        public let bundlePath: String
        public let identifier: String
    }

    /// Records calls to the legacy single-Mach-O `adhocCodesign(path:)`.
    public private(set) var codesignCalls: [String] = []
    /// Records calls to the two-pass `codesignBundle(...)`.
    /// Tests assert on these separately from the single-Mach-O
    /// calls — the bundle path means the fresh-install / upgrade
    /// flow is in use, while the single-Mach-O path is migration-only.
    public private(set) var bundleCodesignCalls: [BundleCodesignCall] = []
    public var pathToReport: String?
    public var failCodesignWith: String?
    /// If non-nil, `codesignBundle` throws with this reason. Lets
    /// tests exercise the bundle-assembly failure path independently
    /// of the single-Mach-O legacy path.
    public var failBundleCodesignWith: String?

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

    public func codesignBundle(bundlePath: String, identifier: String) throws {
        bundleCodesignCalls.append(BundleCodesignCall(
            bundlePath: bundlePath, identifier: identifier))
        if let reason = failBundleCodesignWith {
            throw ExecutableAdapterError.codesignFailed(reason)
        }
    }
}
