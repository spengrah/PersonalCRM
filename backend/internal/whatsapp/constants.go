// Package whatsapp holds the WhatsApp integration. This file is the package's
// first content: the two values that are deliberately NOT environment knobs.
package whatsapp

import "time"

// BackfillFloor is the CRM-wide history horizon. Messages older than this are
// discarded, not stored. Not configurable: a floor earlier than the horizon
// would stage data the rest of the CRM does not carry, and the value is a spec
// hard constraint rather than an operator preference.
const BackfillFloor = "2026-01-01"

// HistorySyncDaysLimit is the history window requested at pairing time. The
// maximum (365) is required because the request is ONE-SHOT: the value is baked
// into the pairing registration payload, so a smaller window is unrecoverable
// without unlinking and re-pairing. Not configurable, for that reason.
const HistorySyncDaysLimit uint32 = 365

// backfillFloorTime is BackfillFloor parsed once. A malformed constant is a
// programmer error caught the first time the package loads, not a runtime
// configuration case, so there is no fallback path.
var backfillFloorTime = mustParseFloor(BackfillFloor)

func mustParseFloor(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic("whatsapp: malformed BackfillFloor constant " + s + ": " + err.Error())
	}
	return t
}

// BackfillFloorTime returns the parsed backfill horizon in UTC. History
// projection clamps against this in memory; nothing older is ever written.
func BackfillFloorTime() time.Time {
	return backfillFloorTime
}
