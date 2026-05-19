// CallCountingContactEnumerator wraps a synthetic
// ContactContainerEnumerator and increments a per-method
// invocation counter. Used by `AllowlistConfigureFlowTests` to
// assert that non-interactive modes make ZERO container-enumeration
// calls (the structural regression guard for shell-context
// Contacts permission errors).
import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

public final class CallCountingContactEnumerator: ContactContainerEnumerator, @unchecked Sendable {
    public var listContainersCalls = 0
    public var stubbedContainers: [ContainerInfo]
    public var thrownError: Error?

    public init(
        containers: [ContainerInfo] = [],
        thrownError: Error? = nil
    ) {
        self.stubbedContainers = containers
        self.thrownError = thrownError
    }

    public func listContainers() throws -> [ContainerInfo] {
        listContainersCalls += 1
        if let err = thrownError { throw err }
        return stubbedContainers
    }
}
