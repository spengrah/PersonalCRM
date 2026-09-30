import Foundation

public enum AnarlogEligibility {
    /// Backfill floor: sessions created earlier are never sent.
    public static let backfillFloor: Date = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.date(from: "2026-01-01T00:00:00Z")!
    }()

    /// Sessions are eligible once they are at least three hours old.
    public static let settleInterval: TimeInterval = 3 * 60 * 60

    /// A session must be on or after the floor and past the settle interval.
    public static func isEligible(createdAt: Date, now: Date) -> Bool {
        createdAt >= backfillFloor && now.timeIntervalSince(createdAt) >= settleInterval
    }
}

extension AnarlogSessionRecord {
    /// Participants other than the operator and the zero-ID sentinel.
    public func participants(excludingOperator operatorPersonID: String) -> [AnarlogParticipant] {
        let excludedOperator = operatorPersonID.lowercased()
        return participants.filter {
            $0.personID != excludedOperator && $0.personID != CRMMacAnarlogSource.selfHumanUUID
        }
    }
}
