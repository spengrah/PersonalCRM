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
//   - CallHistoryScanReader.swift identifier-scoped resumable scan
//   - PayloadShaping.swift        ZCALLRECORD row -> CallPayload
//   - PhoneCallsCursor.swift      typealiases pointing at CRMMacCore wire
//   - PhoneCallsPublisher.swift   /ingest/events batching
//   - PhoneCallsSourcePlugin.swift SourcePlugin actor + tick orchestration
//
// The HandleNormalization helper lives in CRMMacMessagesSource and is
// injected as a closure so this target stays free of a cross-source
// dep. The KnownIdentifiersCache lives in CRMMacCore; both messages
// and phone_calls plugins share a single cache instance via the
// daemon composition root.
import Foundation

public enum CRMMacPhoneCallsSource {
    /// Versioned anchor — bumped when the source ships breaking changes
    /// to the Pi-side payload contract.
    public static let payloadVersion: Int = 1

    /// Minimum Pi-reported `protocol_version` the source requires
    /// before it activates. Protocol version 2 added `call.*`
    /// event-kind support on the Pi side; daemons with this gate
    /// refuse to emit `call.*` events to older Pi instances and
    /// remain operational for the messages + icloud_contacts
    /// sources.
    public static let minPiProtocolVersion: Int32 = 2
}
