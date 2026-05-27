// AnarlogHumanRecord is the daemon's neutral projection of an Anarlog
// `humans/<uuid>.md` file: parsed frontmatter + optional memo body.
//
// Field set per spec line 187. Unknown frontmatter keys are kept in
// `rawExtras` so a future Anarlog version that adds a new key doesn't
// silently lose data (the daemon doesn't ship it yet — but the
// presence in raw form helps with future migrations + debugging).
import Foundation

public struct AnarlogHumanRecord: Equatable, Sendable {
    /// File UUID — derived from the filename, NOT the frontmatter.
    public let uuid: String

    /// Display name. May be empty when the frontmatter omits the key
    /// or carries `''`; callers fall back to `"<no name>"`.
    public let name: String

    /// Emails parsed from the frontmatter array. Empty array when the
    /// key is absent or `[]`.
    public let emails: [String]

    public let jobTitle: String
    public let linkedinUsername: String
    public let orgID: String
    public let userID: String
    public let pinOrder: Int
    public let pinned: Bool

    /// `created_at` parsed via AnarlogTimestampParser; nil when absent
    /// or unparseable (the daemon proceeds without it — the field is
    /// metadata, not load-bearing for the source_id).
    public let createdAt: Date?

    /// Memo body — content after the closing `---` line of the YAML
    /// frontmatter. Trimmed; nil when empty.
    public let memo: String?

    /// Unknown frontmatter keys captured verbatim as `key → raw value
    /// string`. Not currently shipped Pi-side; preserves data for
    /// future migrations.
    public let rawExtras: [String: String]

    public init(
        uuid: String,
        name: String = "",
        emails: [String] = [],
        jobTitle: String = "",
        linkedinUsername: String = "",
        orgID: String = "",
        userID: String = "",
        pinOrder: Int = 0,
        pinned: Bool = false,
        createdAt: Date? = nil,
        memo: String? = nil,
        rawExtras: [String: String] = [:]
    ) {
        self.uuid = uuid
        self.name = name
        self.emails = emails
        self.jobTitle = jobTitle
        self.linkedinUsername = linkedinUsername
        self.orgID = orgID
        self.userID = userID
        self.pinOrder = pinOrder
        self.pinned = pinned
        self.createdAt = createdAt
        self.memo = memo
        self.rawExtras = rawExtras
    }
}
