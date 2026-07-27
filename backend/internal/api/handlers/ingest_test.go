package handlers

import (
	"encoding/json"
	"testing"

	"personal-crm/backend/internal/events"

	"github.com/stretchr/testify/require"
)

// These are unit tests against the per-family payload-version validators
// directly (no HTTP/DB harness needed — they're pure functions operating
// on an IngestEventRequest's raw JSON payload). Both halves of the
// version envelope (below-minimum rejection and above-maximum rejection
// with the "upgrade Pi" signal) are pinned here for ALL FOUR daemon
// families the spec names: message, external-contact, meeting-note, and
// call. The API-integration level additionally exercises the
// external_contact and call version gates end-to-end
// (external_contact_ingest and phone_call_ingest test files).

// spec: ING-005.version-below-supported-minimum
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

// spec: ING-005.version-above-highest-known
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

// spec: ING-005.version-below-supported-minimum
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

// spec: ING-005.version-above-highest-known
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

// spec: ING-005.version-below-supported-minimum
func TestValidateExternalContactPayloadVersion_VersionBelowMin_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "icloud_contacts",
		SourceID: "ext-below-min",
		Kind:     string(events.KindExternalContactUpserted),
		Payload:  json.RawMessage(`{"version":0}`),
	}
	ierr := validateExternalContactPayloadVersion(0, events.KindExternalContactUpserted, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "version")
}

// spec: ING-005.version-above-highest-known
func TestValidateExternalContactPayloadVersion_VersionTooHigh_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "icloud_contacts",
		SourceID: "ext-too-high",
		Kind:     string(events.KindExternalContactUpserted),
		Payload:  json.RawMessage(`{"version":999}`),
	}
	ierr := validateExternalContactPayloadVersion(0, events.KindExternalContactUpserted, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "upgrade Pi")
}

// spec: ING-005.version-below-supported-minimum
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

// spec: ING-005.version-above-highest-known
func TestValidateCallPayloadVersion_VersionTooHigh_Rejected(t *testing.T) {
	ev := IngestEventRequest{
		Source:   "phone_calls",
		SourceID: "call-too-high",
		Kind:     string(events.KindCallReceived),
		Payload:  json.RawMessage(`{"version":999}`),
	}
	ierr := validateCallPayloadVersion(0, events.KindCallReceived, ev)
	require.NotNil(t, ierr)
	require.Equal(t, ingestCodePayloadInvalid, ierr.Code)
	require.Contains(t, ierr.Message, "upgrade Pi")
}
