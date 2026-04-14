package main

import (
	"context"

	"github.com/riverqueue/river"
)

// noopJobArgs is a throwaway job type used only to prove that River's
// AddWorker / client registration wiring works end-to-end in PR 1 of #180.
// No jobs of this kind are ever enqueued at runtime. It will be removed in
// PR 2 once a real consumer worker (the InteractionRecorder wrapper per
// .ai/spec/event-bus-foundation.md §3.4) is registered.
type noopJobArgs struct{}

// Kind returns the River job kind identifier for noopJobArgs.
func (noopJobArgs) Kind() string { return "noop" }

// noopWorker is a no-op River worker paired with noopJobArgs.
type noopWorker struct {
	river.WorkerDefaults[noopJobArgs]
}

// Work implements river.Worker. Since no jobs of this kind are enqueued,
// this method is never called at runtime.
func (*noopWorker) Work(_ context.Context, _ *river.Job[noopJobArgs]) error {
	return nil
}
