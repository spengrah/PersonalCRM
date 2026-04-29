package repository

// FollowUpAction enumerates the four terminal decisions FollowUpManager
// makes for an interaction.recorded event:
//
//   - FollowUpActionCreate:   insert a new follow-up task
//   - FollowUpActionRefresh:  advance an existing pending follow-up's
//     deadline
//   - FollowUpActionComplete: mark a pending follow-up completed on
//     inbound/mutual response
//   - FollowUpActionSkip:     no-op (no cadence / guard fired / no
//     pending on inbound)
//
// Exposed as package-level constants rather than a typed enum so the
// consumer's in-memory Decision observer (used by unit tests) can
// compare values with equality and callers don't need conversion shims.
const (
	FollowUpActionCreate   = "create"
	FollowUpActionRefresh  = "refresh"
	FollowUpActionComplete = "complete"
	FollowUpActionSkip     = "skip"
)

// FollowUpSkipReason enumerates the guard-class skip reasons the manager
// distinguishes on outbound skips. Non-guard-class skips (no-cadence
// outbound, no-pending inbound/mutual) carry an empty reason.
const (
	FollowUpSkipReasonBackdated        = "backdated"
	FollowUpSkipReasonOutOfOrder       = "out_of_order"
	FollowUpSkipReasonDuplicatePending = "duplicate_pending"
	// FollowUpSkipReasonSuppressed fires when InteractionRecordedPayload.SuppressFollowUp
	// is true (set by Todoist provider for kind=send completions).
	FollowUpSkipReasonSuppressed = "suppress_follow_up"
)
