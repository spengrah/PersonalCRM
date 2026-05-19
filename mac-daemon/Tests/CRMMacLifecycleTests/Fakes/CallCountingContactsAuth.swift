// CallCountingContactsAuth wraps a synthetic ContactsAuthorization
// adapter and increments per-method invocation counters. Used by
// `AllowlistConfigureFlowTests` to assert that non-interactive
// modes make ZERO Contacts authorization calls — the regression
// guard for issue #322.
import Foundation
import CRMMacCore

public final class CallCountingContactsAuth: ContactsAuthorizationAdapter, @unchecked Sendable {
    public var authStatusCalls = 0
    public var requestAccessCalls = 0
    public var grantOnRequest: Bool
    public var stubbedStatus: ContactsAuthorizationStatus

    public init(
        stubbedStatus: ContactsAuthorizationStatus = .authorized,
        grantOnRequest: Bool = true
    ) {
        self.stubbedStatus = stubbedStatus
        self.grantOnRequest = grantOnRequest
    }

    public func authorizationStatus() -> ContactsAuthorizationStatus {
        authStatusCalls += 1
        return stubbedStatus
    }

    public func requestAccess() async throws -> Bool {
        requestAccessCalls += 1
        return grantOnRequest
    }
}
