// Package contacttask defines neutral constants for the orthogonal
// (kind, lifecycle) axes on contact_task. Constants only — no DB, no
// handlers — so any package can import without risking cycles.
//
// The two axes are independent dispatch surfaces:
//   - kind tracks the semantic completion behaviour (interaction
//     direction + does it spawn a follow-up).
//   - lifecycle tracks the row's origin/scheduling/dismissal class.
//
// State space is constrained by the contact_task_kind_lifecycle_check
// in migration 046: only kind=reach_out participates in all three
// lifecycles; every other kind is only valid with lifecycle=manual.
package contacttask

// Kind values. User-pickable: reach_out / send / reminder.
// Legacy preserved: meet / action (no new instances created).
const (
	KindReachOut = "reach_out"
	KindSend     = "send"
	KindReminder = "reminder"
	// KindMeet is legacy — preserved for rows that already exist; no new
	// creator path produces it.
	KindMeet = "meet"
	// KindAction is legacy — preserved for rows that already exist; no
	// new creator path produces it.
	KindAction = "action"
)

// Lifecycle values.
const (
	// LifecycleManual: user-created (picker), or legacy action/meet rows.
	LifecycleManual = "manual"
	// LifecycleCadenceDue: scheduler/Todoist provider creates when contact_by reaches now.
	LifecycleCadenceDue = "cadence_due"
	// LifecycleFollowUpLoop: FollowUpManager consumer creates after outbound interaction.
	LifecycleFollowUpLoop = "followup_loop"
)
