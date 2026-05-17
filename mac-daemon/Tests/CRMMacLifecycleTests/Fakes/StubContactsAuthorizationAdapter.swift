import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

/// In-memory stub for ContactsAuthorizationAdapter. Used by Doctor +
/// install + plugin tests; production impl lives in CRMMacSystem.
public final class StubContactsAuthorizationAdapter: ContactsAuthorizationAdapter, @unchecked Sendable {
    public var status: ContactsAuthorizationStatus
    public var grantOnRequest: Bool
    public var requestError: Error?

    public init(
        status: ContactsAuthorizationStatus = .authorized,
        grantOnRequest: Bool = true,
        requestError: Error? = nil
    ) {
        self.status = status
        self.grantOnRequest = grantOnRequest
        self.requestError = requestError
    }

    public func authorizationStatus() -> ContactsAuthorizationStatus { status }

    public func requestAccess() async throws -> Bool {
        if let err = requestError { throw err }
        return grantOnRequest
    }
}

/// In-memory stub for ContactContainerEnumerator.
public final class StubContactContainerEnumerator: ContactContainerEnumerator, @unchecked Sendable {
    public var containers: [ContainerInfo]
    public var thrownError: Error?

    public init(
        containers: [ContainerInfo] = [],
        thrownError: Error? = nil
    ) {
        self.containers = containers
        self.thrownError = thrownError
    }

    public func listContainers() throws -> [ContainerInfo] {
        if let err = thrownError { throw err }
        return containers
    }
}
