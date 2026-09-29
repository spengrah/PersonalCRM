import CRMMacCore
import Foundation

public struct AnarlogSessionListEntry: Sendable, Equatable {
    public let id: String            // lowercase UUID string
    public let createdAt: Date

    public init(id: String, createdAt: Date) {
        self.id = id
        self.createdAt = createdAt
    }
}

public struct AnarlogParticipant: Sendable, Equatable {
    public let personID: String      // lowercase UUID string
    public let displayName: String?
    public let email: String?
    public let jobTitle: String?

    public init(personID: String, displayName: String?, email: String?, jobTitle: String?) {
        self.personID = personID
        self.displayName = displayName
        self.email = email
        self.jobTitle = jobTitle
    }
}

public struct AnarlogSessionRecord: Sendable, Equatable {
    public let id: String
    public let title: String?
    public let createdAt: Date
    public let memo: String?         // markdown, as the CLI returns it
    public let summaries: [String]   // markdown, in CLI order; default or template output
    public let participants: [AnarlogParticipant]  // unfiltered

    public init(
        id: String,
        title: String?,
        createdAt: Date,
        memo: String?,
        summaries: [String],
        participants: [AnarlogParticipant]
    ) {
        self.id = id
        self.title = title
        self.createdAt = createdAt
        self.memo = memo
        self.summaries = summaries
        self.participants = participants
    }
}

public protocol AnarlogCLIClient: Sendable {
    /// The complete live listing, every page, or a failure. Never partial.
    func listSessions() async throws(AnarlogCLIFailure) -> [AnarlogSessionListEntry]
    /// nil when the CLI answers not_found.
    func getSession(id: String) async throws(AnarlogCLIFailure) -> AnarlogSessionRecord?
}
