// PhoneCallsCursor — typealiases pointing at the shared
// CRMMacCore.PhoneCallsCursorWire definition.
//
// The wire types live in CRMMacCore so both this target and CLI ops
// subcommands can share the same source-of-truth schema. Drift between
// two redeclared types would silently break the daemon <-> ops cursor
// round-trip.
import Foundation
import CRMMacCore

public typealias PhoneCallsCursor = PhoneCallsCursorWire
public typealias PhoneCallsCursorCodec = PhoneCallsCursorWireCodec
