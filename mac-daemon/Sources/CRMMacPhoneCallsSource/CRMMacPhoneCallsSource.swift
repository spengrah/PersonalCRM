// CRMMacPhoneCallsSource — the CallHistoryDB reader +
// PhoneCallsSourcePlugin.
//
// Target layering: depends on CRMMacCore, CRMMacPiClient, and
// GRDB.swift. Isolated from other targets so GRDB is not transitively
// pulled into Foundation-only modules (CRMMacCore, CRMMacLifecycle).
//
// Source files:
//   - ServiceDerivation.swift     (ZSERVICE_PROVIDER, ZCALLTYPE) -> service
//   - CallHistoryDBSchema.swift   whitelist + drift detection
//   - CallHistoryDBReader.swift   GRDB-backed row iterator
//   - PayloadShaping.swift        ZCALLRECORD row -> CallPayload
//   - PhoneCallsCursor.swift      typealiases pointing at CRMMacCore wire
//   - PhoneCallsPublisher.swift   /ingest/events batching
//   - PhoneCallsSourcePlugin.swift SourcePlugin actor + tick orchestration
//
// The HandleNormalization helper lives in CRMMacMessagesSource and is
// re-used here (peer canonicalization on the daemon side per S R2-P2-H).
// The KnownIdentifiersCache moved to CRMMacCore in this PR's earlier
// commit; both messages + phone_calls plugins share a single cache
// instance.
import Foundation

public enum CRMMacPhoneCallsSource {
    /// Versioned anchor — bumped when the source ships breaking changes
    /// to the Pi-side payload contract.
    public static let payloadVersion: Int = 1

    /// Minimum Pi-reported `protocol_version` the source requires before
    /// it activates. Pi PR 1.5 bumped its `ProtocolVersion` constant to
    /// 2 to advertise `call.*` event-kind support; daemons with this
    /// gate refuse to emit `call.*` events to older Pi instances.
    public static let minPiProtocolVersion: Int32 = 2
}
