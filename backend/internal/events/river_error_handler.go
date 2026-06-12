package events

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
)

// RiverErrorHandler is a client-level River ErrorHandler that logs every errored
// attempt and panic into the app's zerolog stream. It is a pure observer: both
// methods always return nil, so River's configured retry schedule is untouched.
//
// A single client-level handler covers every job kind, including the event-bus
// spec §3.4.3 promise that "an admin alert log fires" when a
// todoist_followup_create job exhausts its attempts — no per-worker log calls
// are needed.
type RiverErrorHandler struct {
	zl *zerolog.Logger
}

// NewRiverErrorHandler builds a RiverErrorHandler over the given logger. main.go
// passes logger.Get(); the logger is initialized at boot before the River client
// is constructed.
func NewRiverErrorHandler(zl *zerolog.Logger) *RiverErrorHandler {
	return &RiverErrorHandler{zl: zl}
}

// HandleError logs an errored job attempt. The attempt that just failed is the
// final one (the job will be discarded) iff Attempt >= MaxAttempts — the exact
// predicate River applies after this handler returns. Discards log at ERROR;
// retryable failures log at WARN. Always returns nil (pure observer).
func (h *RiverErrorHandler) HandleError(_ context.Context, job *rivertype.JobRow, err error) *river.ErrorHandlerResult {
	discarded := job.Attempt >= job.MaxAttempts
	level := zerolog.WarnLevel
	msg := "river job errored; will retry"
	if discarded {
		level = zerolog.ErrorLevel
		msg = "river job discarded after final attempt"
	}
	h.baseEvent(level, job, discarded).Err(err).Msg(msg)
	return nil
}

// HandlePanic logs a panicking job attempt. A panic always logs at ERROR
// regardless of remaining retry budget — a panic is a code bug, and a panicking
// worker usually panics on every attempt, so deferring the signal would only
// delay it. The discarded field still distinguishes a final-attempt panic.
// Always returns nil (pure observer).
func (h *RiverErrorHandler) HandlePanic(_ context.Context, job *rivertype.JobRow, panicVal any, trace string) *river.ErrorHandlerResult {
	discarded := job.Attempt >= job.MaxAttempts
	h.baseEvent(zerolog.ErrorLevel, job, discarded).
		Str("panic", fmt.Sprintf("%v", panicVal)).
		Str("trace", trace).
		Msg("river job panicked")
	return nil
}

// baseEvent builds a zerolog event with the fields common to both error and
// panic logs. The caller appends its own fields (error / panic+trace) and the
// message.
func (h *RiverErrorHandler) baseEvent(level zerolog.Level, job *rivertype.JobRow, discarded bool) *zerolog.Event {
	event := h.zl.WithLevel(level).
		Int64("job_id", job.ID).
		Str("kind", job.Kind).
		Str("queue", job.Queue).
		Int("attempt", job.Attempt).
		Int("max_attempts", job.MaxAttempts).
		Bool("discarded", discarded)
	// EncodedArgs is always valid JSON for River-inserted rows; guard the
	// defensive empty case so the log record can't be corrupted by RawJSON on
	// an empty slice.
	if len(job.EncodedArgs) == 0 {
		event.Str("args", "")
	} else {
		event.RawJSON("args", job.EncodedArgs)
	}
	return event
}
