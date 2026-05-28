// FakeSessionMetadataLookup — returns canned SessionMetadata
// for specific UUIDs; nil for unknown UUIDs.
import Foundation
@testable import CRMMacOrphanNotifications

public actor FakeSessionMetadataLookup: SessionMetadataLookup {
    private var canned: [String: SessionMetadata]
    public private(set) var lookupCalls: [String] = []

    public init(canned: [String: SessionMetadata] = [:]) {
        self.canned = canned
    }

    public func setCanned(_ value: [String: SessionMetadata]) {
        self.canned = value
    }

    public func recordedLookups() -> [String] { lookupCalls }

    public func lookup(sessionUUID: String) async -> SessionMetadata? {
        lookupCalls.append(sessionUUID)
        return canned[sessionUUID]
    }
}
