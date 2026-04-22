package repository

// CadenceBranchForward / CadenceBranchUnconditional select which cadence
// UPDATE SQL the CadenceUpdater dispatches. Branch values mirror the
// CHECK constraint string values in the original cadence observation
// table migration (039) so any historical rows keep the same wire-level
// branch label.
//
//   - CadenceBranchForward:       forward-only; each cadence column is
//     updated only if the incoming value is strictly newer than the
//     existing value. Used by interaction-driven paths (spec §3.4.2).
//   - CadenceBranchUnconditional: the incoming value is written as-is,
//     allowing clears and backdates. Used by user-driven cadence edits
//     (manual source + ApplyContactByOverride) and by manual-source
//     interaction-driven paths.
const (
	CadenceBranchForward       = "forward"
	CadenceBranchUnconditional = "unconditional"
)
