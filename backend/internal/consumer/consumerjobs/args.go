// Package consumerjobs holds leaf-level River job-arg structs shared by
// the events package (enqueue site, via Bus.consumerJobsForKind) and the
// consumer package (worker definitions). Extracting these into a separate
// package avoids an import cycle: events imports consumerjobs to enqueue
// jobs; consumer imports consumerjobs to type its workers; consumer also
// imports events; but events never imports consumer.
//
// Each consumer worker's JobArgs type lives here.
package consumerjobs

import (
	"time"

	"github.com/google/uuid"
)

// InteractionRecorderJobArgs carries the event id that the
// InteractionRecorder worker should fetch and process. Keeping the arg
// minimal (just the event id) matches spec §3.3: worker fetches the full
// envelope via bus.GetEvent at dequeue time rather than serializing the
// whole payload into the river job row.
//
// EventID carries the `river:"unique"` tag so the aggregator's stale-
// claim recovery path (which enqueues with UniqueOpts{ByArgs: true})
// dedupes repeated recovery enqueues against the same event into one
// in-flight job. The publish-side enqueue (from
// events.consumerJobsForKind) does NOT pass UniqueOpts, so the tag is
// a no-op there — only the recovery path consults it. Default ByState
// (Pending/Scheduled/Available/Running/Retryable) excludes `discarded`,
// so a permanently-failing consumer's MaxAttempts exhaustion
// eventually frees a fresh recovery slot.
type InteractionRecorderJobArgs struct {
	EventID uuid.UUID `json:"event_id" river:"unique"`
}

// Kind returns the river job-kind identifier used for registration and
// dequeue routing.
func (InteractionRecorderJobArgs) Kind() string { return "interaction_recorder" }

// CadenceUpdaterJobArgs carries the event id that the CadenceUpdater
// worker should fetch and process. Enqueued for every interaction.recorded
// event by events.consumerJobsForKind.
type CadenceUpdaterJobArgs struct {
	EventID uuid.UUID `json:"event_id"`
}

// Kind returns the river job-kind identifier for CadenceUpdater jobs.
func (CadenceUpdaterJobArgs) Kind() string { return "cadence_updater" }

// FollowUpManagerJobArgs carries the event id that the FollowUpManager
// worker should fetch and process. Enqueued alongside CadenceUpdater for
// every interaction.recorded event. Routing is config-blind — the
// mode-gate runs inside FollowUpManager.HandleEvent so the worker
// completes with zero side effects when mode=off.
type FollowUpManagerJobArgs struct {
	EventID uuid.UUID `json:"event_id"`
}

// Kind returns the river job-kind identifier for FollowUpManager jobs.
func (FollowUpManagerJobArgs) Kind() string { return "followup_manager" }

// TodoistFollowUpCreateJobArgs carries the contact_task id the Todoist
// follow-up create worker should process. Enqueued only by the cutover
// consumer; in shadow mode the worker body returns an error when
// invoked because no code path enqueues it.
type TodoistFollowUpCreateJobArgs struct {
	ContactTaskID uuid.UUID `json:"contact_task_id"`
}

// Kind returns the river job-kind identifier for Todoist follow-up
// create jobs.
func (TodoistFollowUpCreateJobArgs) Kind() string { return "todoist_followup_create" }

// TodoistFollowUpCloseJobArgs carries the contact_task id the Todoist
// follow-up close worker should process. Enqueued only by the cutover
// consumer when a follow-up is completed; in shadow mode the worker
// body returns an error when invoked.
type TodoistFollowUpCloseJobArgs struct {
	ContactTaskID uuid.UUID `json:"contact_task_id"`
}

// Kind returns the river job-kind identifier for Todoist follow-up
// close jobs.
func (TodoistFollowUpCloseJobArgs) Kind() string { return "todoist_followup_close" }

// TodoistFollowUpRefreshJobArgs carries the contact_task id + new
// deadline for a retryable Todoist item_update on a follow-up task.
// The cutover FollowUpManager runs the refresh call once inline from
// its post-commit closure; on failure the closure enqueues this job
// for river-managed retry with MaxAttempts=10 exponential backoff.
type TodoistFollowUpRefreshJobArgs struct {
	ContactTaskID uuid.UUID `json:"contact_task_id"`
	NewDeadline   time.Time `json:"new_deadline"`
}

// Kind returns the river job-kind identifier for Todoist follow-up
// refresh retry jobs.
func (TodoistFollowUpRefreshJobArgs) Kind() string { return "todoist_followup_refresh" }

// MessagingAggregateForContactArgs is enqueued by the ingest service
// after a batch of raw_message.* events lands. The worker drives the
// chat-aware aggregation engine over all unprocessed chats for the
// (contactID, source) pair.
//
// ContactID + Source carry the `river:"unique"` tag so River's
// UniqueOpts{ByArgs: true} dedupes concurrent enqueues for the same
// pair into one in-flight job. Default ByState (Pending/Scheduled/
// Available/Running/Retryable) is intentional — completed jobs do
// NOT block re-enqueue, so a subsequent batch arriving after the
// previous worker finished still triggers a fresh aggregation pass.
//
// JSON tag names ("contact_id" / "source") are load-bearing for
// River's args-hash uniqueness; do not rename without auditing all
// consumers/tests that decode the args.
type MessagingAggregateForContactArgs struct {
	ContactID uuid.UUID `json:"contact_id" river:"unique"`
	Source    string    `json:"source"     river:"unique"`
}

// Kind returns the river job-kind identifier for messaging aggregate
// jobs.
func (MessagingAggregateForContactArgs) Kind() string { return "messaging_aggregate_for_contact" }

// MessagingAggregateSweeperArgs is the periodic sweep job. The worker
// lists all contacts with unprocessed messages_message rows and
// enqueues a MessagingAggregateForContactArgs per contact, relying on
// UniqueOpts to dedupe against in-flight jobs. Bounds the worst-case
// stranded-row latency at the sweep interval. The worker holds no
// state; the args type is intentionally empty.
type MessagingAggregateSweeperArgs struct{}

// Kind returns the river job-kind identifier for the sweeper.
func (MessagingAggregateSweeperArgs) Kind() string { return "messaging_aggregate_sweeper" }

// RematchDispatcherJobArgs carries the event id + dedup-arg fields that
// the RematchDispatcher worker processes. ContactID and RematchJobID
// are duplicated from the event payload and carry the river:"unique"
// struct tag so river's UniqueOpts{ByArgs: true} dedupes same-mutation
// publisher retries at enqueue time without decoding the payload. The
// event.source_id unique index is the first dedup layer (publisher-
// retry side); river args-based uniqueness is the belt-and-suspenders
// second layer (river-enqueue side). EventID is intentionally NOT
// tagged — two retries of the same mutation produce distinct event
// rows (different event.id) but identical (ContactID, RematchJobID),
// so only those two fields participate in the uniqueness hash.
// Spec §3.4.4.
type RematchDispatcherJobArgs struct {
	EventID      uuid.UUID `json:"event_id"`
	ContactID    uuid.UUID `json:"contact_id"    river:"unique"`
	RematchJobID uuid.UUID `json:"rematch_job_id" river:"unique"`
}

// Kind returns the river job-kind identifier for RematchDispatcher jobs.
func (RematchDispatcherJobArgs) Kind() string { return "rematch_dispatcher" }
