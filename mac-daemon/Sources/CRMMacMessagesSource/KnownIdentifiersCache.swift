// KnownIdentifiersCache + KnownIdentifiersHash were moved up to
// CRMMacCore in phase 1.5 so the phone_calls source plugin can share
// the same actor + hash helper without duplication. This file is now a
// re-export shim so existing CRMMacMessagesSource imports keep
// compiling without site-by-site changes. Production code paths in
// the messages plugin construct the cache via these typealiases; the
// runtime instance is wired at the composition root and passed to
// both plugins.
//
// Do not add behavior here — extend CRMMacCore.KnownIdentifiersCache
// instead so both sources stay in lockstep.
@_exported import CRMMacCore

public typealias KnownIdentifiersCache = CRMMacCore.KnownIdentifiersCache
public typealias KnownIdentifiersHash = CRMMacCore.KnownIdentifiersHash
