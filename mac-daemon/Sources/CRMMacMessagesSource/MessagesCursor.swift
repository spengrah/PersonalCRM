// MessagesCursor — typealiases pointing at the shared
// CRMMacCore.MessagesCursorWire definition.
//
// The wire types live in CRMMacCore so both this target and
// CRMMacLifecycle (which produces + consumes the cursor JSON in the
// CLI ops subcommands) can share the same source-of-truth schema.
// Drift between two redeclared types would silently break the
// daemon <-> ops cursor round-trip.
import Foundation
import CRMMacCore

public typealias MessagesCursor = MessagesCursorWire
public typealias PendingScan = MessagesCursorPendingScan
public typealias MessagesCursorCodec = MessagesCursorWireCodec
