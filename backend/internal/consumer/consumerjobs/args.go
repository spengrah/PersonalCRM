// Package consumerjobs holds leaf-level River job-arg structs shared by
// the events package (enqueue site, via Bus.consumerJobsForKind) and the
// consumer package (worker definitions). Extracting these into a separate
// package avoids an import cycle: events imports consumerjobs to enqueue
// jobs; consumer imports consumerjobs to type its workers; consumer also
// imports events; but events never imports consumer.
//
// Each consumer worker's JobArgs type lives here. PR 5 adds
// InteractionRecorderJobArgs; later PRs (7, 9a, 10) will add CadenceUpdater,
// FollowUpManager, and RematchDispatcher args.
package consumerjobs

import "github.com/google/uuid"

// InteractionRecorderJobArgs carries the event id that the
// InteractionRecorder worker should fetch and process. Keeping the arg
// minimal (just the event id) matches spec §3.3: worker fetches the full
// envelope via bus.GetEvent at dequeue time rather than serializing the
// whole payload into the river job row.
type InteractionRecorderJobArgs struct {
	EventID uuid.UUID `json:"event_id"`
}

// Kind returns the river job-kind identifier used for registration and
// dequeue routing.
func (InteractionRecorderJobArgs) Kind() string { return "interaction_recorder" }
