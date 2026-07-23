package handlers

import (
	"encoding/json"
	"testing"

	"personal-crm/backend/internal/events"

	"github.com/stretchr/testify/require"
)

// These are unit tests against the per-family payload-version validators
// directly (no HTTP/DB harness needed — they're pure functions operating
// on an IngestEventRequest's raw JSON payload). validateCallPayloadVersion
// and validateExternalContactPayloadVersion already have below/above
// version coverage at the API-integration level (phone_call_ingest and
// external_contact_ingest test files); raw_message and meeting_note don't,
// so both halves are covered here for those two families, plus the
// below-minimum half for call (rounding out ING-005[0] across all three
// families named in the spec).

// spec: ING-005[0]
func TestValidateRawMessagePayload_VersionBelowMin_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "messages",
		SourceID: "guid-below-min",
		Kind:     string(events.KindRawMessageReceived),
		Payload:  json.RawMessage(`{"version":0}`),
	}
	ierr := validateRawMessagePayload(0, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "version")
}

// spec: ING-005[1]
func TestValidateRawMessagePayload_VersionTooHigh_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "messages",
		SourceID: "guid-too-high",
		Kind:     string(events.KindRawMessageReceived),
		Payload:  json.RawMessage(`{"version":999}`),
	}
	ierr := validateRawMessagePayload(0, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "upgrade Pi")
}

// spec: ING-005[0]
func TestValidateMeetingNotePayloadVersion_VersionBelowMin_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "anarlog_sessions",
		SourceID: "session-below-min",
		Kind:     string(events.KindMeetingNoteRecorded),
		Payload:  json.RawMessage(`{"version":0}`),
	}
	ierr := validateMeetingNotePayloadVersion(0, events.KindMeetingNoteRecorded, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "version")
}

// spec: ING-005[1]
func TestValidateMeetingNotePayloadVersion_VersionTooHigh_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "anarlog_sessions",
		SourceID: "session-too-high",
		Kind:     string(events.KindMeetingNoteRecorded),
		Payload:  json.RawMessage(`{"version":999}`),
	}
	ierr := validateMeetingNotePayloadVersion(0, events.KindMeetingNoteRecorded, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "upgrade Pi")
}

// spec: ING-005[0]
func TestValidateCallPayloadVersion_VersionBelowMin_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "phone_calls",
		SourceID: "call-below-min",
		Kind:     string(events.KindCallReceived),
		Payload:  json.RawMessage(`{"version":0}`),
	}
	ierr := validateCallPayloadVersion(0, events.KindCallReceived, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "version")
}
