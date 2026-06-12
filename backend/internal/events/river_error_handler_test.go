package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBufferHandler builds a RiverErrorHandler writing JSON into the returned
// buffer at debug level (so every record is emitted). Parallel-safe: no global
// mutation.
func newBufferHandler() (*RiverErrorHandler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	zl := zerolog.New(buf).Level(zerolog.DebugLevel).With().Timestamp().Logger()
	return NewRiverErrorHandler(&zl), buf
}

func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	require.NotEmpty(t, buf.Bytes(), "expected a log line")
	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m))
	return m
}

func TestHandleError_RetryableLogsWarn(t *testing.T) {
	t.Parallel()
	h, buf := newBufferHandler()
	job := &rivertype.JobRow{
		ID:          101,
		Kind:        "interaction_recorder",
		Queue:       "default",
		Attempt:     3,
		MaxAttempts: 5,
		EncodedArgs: []byte(`{"event_id":"00000000-0000-0000-0000-000000000001"}`),
	}
	res := h.HandleError(context.Background(), job, errors.New("transient db error"))
	assert.Nil(t, res, "handler must always return nil (pure observer)")

	m := decodeRecord(t, buf)
	assert.Equal(t, "warn", m["level"])
	assert.Equal(t, false, m["discarded"])
	assert.Equal(t, float64(101), m["job_id"])
	assert.Equal(t, "interaction_recorder", m["kind"])
	assert.Equal(t, "default", m["queue"])
	assert.Equal(t, float64(3), m["attempt"])
	assert.Equal(t, float64(5), m["max_attempts"])
	assert.Equal(t, "transient db error", m["error"])
	assert.Equal(t, "river job errored; will retry", m["message"])
	// args round-trips as the raw JSON object.
	args, ok := m["args"].(map[string]any)
	require.True(t, ok, "args should be a JSON object")
	assert.Equal(t, "00000000-0000-0000-0000-000000000001", args["event_id"])
}

func TestHandleError_FinalAttemptLogsErrorDiscarded(t *testing.T) {
	t.Parallel()
	h, buf := newBufferHandler()
	job := &rivertype.JobRow{
		ID:          102,
		Kind:        "cadence_updater",
		Queue:       "default",
		Attempt:     5,
		MaxAttempts: 5,
		EncodedArgs: []byte(`{"contact_id":"00000000-0000-0000-0000-000000000002"}`),
	}
	res := h.HandleError(context.Background(), job, errors.New("permanent failure"))
	assert.Nil(t, res)

	m := decodeRecord(t, buf)
	assert.Equal(t, "error", m["level"])
	assert.Equal(t, true, m["discarded"])
	assert.Equal(t, "river job discarded after final attempt", m["message"])
	assert.Equal(t, "permanent failure", m["error"])
}

// TestHandleError_TodoistFollowupExhaustion pins the event-bus spec §3.4.3 case:
// a todoist_followup_create job exhausting MaxAttempts=10 logs at ERROR with
// discarded=true — the generic implementation of the spec's "admin alert log
// fires" promise.
func TestHandleError_TodoistFollowupExhaustion(t *testing.T) {
	t.Parallel()
	h, buf := newBufferHandler()
	job := &rivertype.JobRow{
		ID:          103,
		Kind:        "todoist_followup_create",
		Queue:       "default",
		Attempt:     10,
		MaxAttempts: 10,
		EncodedArgs: []byte(`{"contact_task_id":"00000000-0000-0000-0000-000000000003"}`),
	}
	res := h.HandleError(context.Background(), job, errors.New("POST /tasks: 503"))
	assert.Nil(t, res)

	m := decodeRecord(t, buf)
	assert.Equal(t, "error", m["level"])
	assert.Equal(t, true, m["discarded"])
	assert.Equal(t, "todoist_followup_create", m["kind"])
	assert.Equal(t, float64(10), m["attempt"])
	assert.Equal(t, float64(10), m["max_attempts"])
}

func TestHandlePanic_NonFinalLogsErrorWithTrace(t *testing.T) {
	t.Parallel()
	h, buf := newBufferHandler()
	job := &rivertype.JobRow{
		ID:          104,
		Kind:        "interaction_recorder",
		Queue:       "default",
		Attempt:     1,
		MaxAttempts: 3,
		EncodedArgs: []byte(`{"event_id":"00000000-0000-0000-0000-000000000004"}`),
	}
	res := h.HandlePanic(context.Background(), job, "nil map write", "goroutine 1 [running]:\nmain.work()")
	assert.Nil(t, res)

	m := decodeRecord(t, buf)
	// A panic always logs ERROR, even on a non-final attempt.
	assert.Equal(t, "error", m["level"])
	assert.Equal(t, false, m["discarded"])
	assert.Equal(t, "nil map write", m["panic"])
	assert.Contains(t, m["trace"], "goroutine 1")
	assert.Equal(t, "river job panicked", m["message"])
}

// TestHandlePanic_FinalAttemptStaysDistinguishable pins that a final-attempt
// panic still carries discarded=true even though panics always log ERROR.
func TestHandlePanic_FinalAttemptStaysDistinguishable(t *testing.T) {
	t.Parallel()
	h, buf := newBufferHandler()
	job := &rivertype.JobRow{
		ID:          105,
		Kind:        "cadence_updater",
		Queue:       "default",
		Attempt:     3,
		MaxAttempts: 3,
		EncodedArgs: []byte(`{"contact_id":"00000000-0000-0000-0000-000000000005"}`),
	}
	res := h.HandlePanic(context.Background(), job, "index out of range", "trace")
	assert.Nil(t, res)

	m := decodeRecord(t, buf)
	assert.Equal(t, "error", m["level"])
	assert.Equal(t, true, m["discarded"])
}

// TestHandleError_EmptyArgsStaysValidJSON pins the D1 guard: an empty
// EncodedArgs slice produces args="" rather than corrupting the record.
func TestHandleError_EmptyArgsStaysValidJSON(t *testing.T) {
	t.Parallel()
	h, buf := newBufferHandler()
	job := &rivertype.JobRow{
		ID:          106,
		Kind:        "some_kind",
		Queue:       "default",
		Attempt:     1,
		MaxAttempts: 3,
		EncodedArgs: nil,
	}
	res := h.HandleError(context.Background(), job, errors.New("boom"))
	assert.Nil(t, res)

	// The record must still be valid JSON.
	m := decodeRecord(t, buf)
	assert.Equal(t, "", m["args"])
	assert.Equal(t, "warn", m["level"])
}
