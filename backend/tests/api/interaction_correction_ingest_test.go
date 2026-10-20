//go:build integration_testdb

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests make corrections through the correction routes and then
// re-process the corrected source objects through POST /api/v1/ingest/events
// (calls, Anarlog sessions) and POST /api/v1/meeting-notes/:id/resolve-link.

// cadenceRow reads a contact's cadence fields, without updated_at.
func cadenceRow(t *testing.T, contacts *repository.ContactRepository, id uuid.UUID) repository.Contact {
	t.Helper()
	c, err := contacts.GetContact(context.WithoutCancel(t.Context()), id)
	require.NoError(t, err)
	c.UpdatedAt = time.Time{}
	return *c
}

// excludedFrom requires that the contact has no effective participation in
// the interaction holding node.
func excludedFrom(t *testing.T, e *correctionEnv, contactID, node uuid.UUID) {
	t.Helper()
	for _, ep := range e.correctable(t, contactID) {
		require.NotEqual(t, node, *ep.InteractionNodeID, "the removed participation is not restored")
	}
	require.NotContains(t, participantIDs(t, e, node), contactID)
}

func TestInteractionCorrection_HoldsThroughCallIngest(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()

	t.Run("a re-ingested removed call stays removed", func(t *testing.T) {
		// spec: ING-040.no-sync-restores-correction, IXN-018.re-observation-stays-removed
		e := newCorrectionEnv(t)
		env := setupPhoneCallIngestEnvOn(t, e.database)
		callID := env.sourcePrefix + "removed"
		ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, nil)
		require.Equal(t, 1, parseIngestResp(t, postIngestCall(t, env, map[string]any{"events": []any{ev}})).Accepted)
		row, err := env.interactionRpo.FindBySourceRef(t.Context(), env.seededContact, repository.InteractionSourcePhoneCalls, callID)
		require.NoError(t, err)
		e.remove(t, *row.InteractionNodeID, removalBody(service.RemovalReasonDidNotHappen, env.seededContact))
		before := cadenceRow(t, env.contactRepo, env.seededContact)

		// A call's envelope source id is its call id, so the daemon's re-send is
		// acknowledged as a duplicate.
		resp := parseIngestResp(t, postIngestCall(t, env, map[string]any{"events": []any{ev}}))
		require.Equal(t, 1, resp.Duplicate)

		excludedFrom(t, e, env.seededContact, *row.InteractionNodeID)
		require.Equal(t, before, cadenceRow(t, env.contactRepo, env.seededContact))
	})

	t.Run("an outbound whose only later response was removed gets the follow-up the rules give", func(t *testing.T) {
		// spec: ING-040.excluded-response-is-no-response
		e := newCorrectionEnv(t)
		env := setupPhoneCallIngestEnvOn(t, e.database, func(context.Context) (*todoist.Settings, string, error) {
			return &todoist.Settings{ProjectID: "proj", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
		})
		seeded, err := env.contactRepo.GetContact(t.Context(), env.seededContact)
		require.NoError(t, err)
		monthly := "monthly"
		_, err = env.contactRepo.UpdateContact(t.Context(), env.seededContact, repository.UpdateContactRequest{FullName: seeded.FullName, Cadence: &monthly})
		require.NoError(t, err)

		now := accelerated.GetCurrentTime()
		replyID := env.sourcePrefix + "reply"
		reply := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, replyID, env.seededPhone, func(p *events.CallPayload) {
			p.StartedAt = now.Add(-time.Hour)
		})
		require.Equal(t, 1, parseIngestResp(t, postIngestCall(t, env, map[string]any{"events": []any{reply}})).Accepted)
		replyRow, err := env.interactionRpo.FindBySourceRef(t.Context(), env.seededContact, repository.InteractionSourcePhoneCalls, replyID)
		require.NoError(t, err)
		e.remove(t, *replyRow.InteractionNodeID, removalBody(service.RemovalReasonDidNotHappen, env.seededContact))

		outbound := buildCallEvent(t, events.KindCallSent, env.pairedHostID, env.sourcePrefix+"outbound", env.seededPhone, func(p *events.CallPayload) {
			p.StartedAt = now.Add(-2 * time.Hour)
			p.DurationSeconds = 0
		})
		require.Equal(t, 1, parseIngestResp(t, postIngestCall(t, env, map[string]any{"events": []any{outbound}})).Accepted)

		tasks, err := e.tasks.ListContactTasksByContact(t.Context(), env.seededContact)
		require.NoError(t, err)
		var followUps int
		for _, task := range tasks {
			if task.Lifecycle == contacttask.LifecycleFollowUpLoop {
				followUps++
			}
		}
		require.Equal(t, 1, followUps, "with the reply removed, the outbound awaits a reply")
	})
}

func TestInteractionCorrection_HoldsThroughAnarlogIngest(t *testing.T) {
	testsupport.RequireDatabase(t)

	t.Run("a session re-linked by ingest leaves a removed participant excluded", func(t *testing.T) {
		// spec: ING-040.no-sync-restores-correction
		e := newCorrectionEnv(t)
		env := setupMeetingNoteIngestEnvOn(t, e.database)
		suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
		anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
		anarlogB := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-1-%s@example.invalid", suffix))
		sessionUUID := env.newSessionUUID()
		meetingAt := env.uniqueWindowBase(800)
		env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactC})
		ixnPostSession(t, env, sessionUUID, meetingAt, "Relink", []string{anarlogA})
		walkin := ixnSessionRows(t, env, sessionUUID)["anarlog:"+sessionUUID+":walkin:"+env.contactA.String()]
		node := *walkin.InteractionNodeID
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, env.contactA))
		before := cadenceRow(t, env.contactRepo, env.contactA)

		env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})
		ixnPostSession(t, env, sessionUUID, meetingAt.Add(10*time.Minute), "Relink again", []string{anarlogA, anarlogB})

		after := ixnSessionRows(t, env, sessionUUID)["anarlog:"+sessionUUID+":walkin:"+env.contactA.String()]
		require.Equal(t, walkin.ID, after.ID, "the re-linked observation is still recorded")
		require.NotEqual(t, *walkin.VenueID, *after.VenueID)
		excludedFrom(t, e, env.contactA, node)
		require.Equal(t, before, cadenceRow(t, env.contactRepo, env.contactA))
	})

	t.Run("a session re-linked by the user's link resolution leaves a removed participant excluded", func(t *testing.T) {
		// spec: ING-040.no-sync-restores-correction
		e := newCorrectionEnv(t)
		env := setupMeetingNoteIngestEnvOn(t, e.database)
		suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
		anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
		sessionUUID := env.newSessionUUID()
		meetingAt := env.uniqueWindowBase(801)
		ixnPostSession(t, env, sessionUUID, meetingAt, "Resolve", []string{anarlogA})
		tagged := ixnSessionRows(t, env, sessionUUID)["anarlog:"+sessionUUID+":"+env.contactA.String()]
		node := *tagged.InteractionNodeID
		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, env.contactA))
		before := cadenceRow(t, env.contactRepo, env.contactA)

		eventB := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactB})
		env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactC})
		ixnPostSession(t, env, sessionUUID, meetingAt, "Resolve in doubt", []string{anarlogA})
		row := findMeetingNoteRow(t, env, sessionUUID)
		w := postResolveLink(t, env, row.ID.String(), map[string]any{"action": "link", "kind": "event", "id": eventB.String()})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		walkin, ok := ixnSessionRows(t, env, sessionUUID)["anarlog:"+sessionUUID+":walkin:"+env.contactA.String()]
		require.True(t, ok, "the re-linked participant's observation is recorded")
		require.Equal(t, node, *walkin.InteractionNodeID)
		excludedFrom(t, e, env.contactA, node)
		require.Equal(t, before, cadenceRow(t, env.contactRepo, env.contactA))
	})

	t.Run("ingest acknowledges an event whose observation a correction excludes, and dedupes its re-delivery", func(t *testing.T) {
		// spec: ING-040.excluded-evidence-acknowledged, IXN-018.everyone-covers-later-observed
		e := newCorrectionEnv(t)
		env := setupMeetingNoteIngestEnvOn(t, e.database)
		suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
		anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
		anarlogB := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-1-%s@example.invalid", suffix))
		sessionUUID := env.newSessionUUID()
		meetingAt := env.uniqueWindowBase(802)
		ixnPostSession(t, env, sessionUUID, meetingAt, "Cancelled", []string{anarlogA})
		node := *ixnSessionRows(t, env, sessionUUID)["anarlog:"+sessionUUID+":"+env.contactA.String()].InteractionNodeID
		e.remove(t, node, everyoneBody(service.RemovalReasonDidNotHappen))
		beforeB := cadenceRow(t, env.contactRepo, env.contactB)

		cursorPath := "/api/v1/host/" + env.pairedHostID.String() + "/sync/anarlog_sessions/cursor"
		var cur struct {
			Cursor      string `json:"cursor"`
			CursorEpoch int64  `json:"cursor_epoch"`
		}
		readData(t, mnHostHTTP(t, env, http.MethodGet, cursorPath, nil), &cur)
		base := cur.Cursor

		ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Cancelled", []string{anarlogA, anarlogB})
		first := parseMNIngestResp(t, postMNIngest(t, env, map[string]any{"events": []any{ev}}))
		require.Equal(t, 1, first.Accepted, "errors=%+v", first.Errors)
		require.Zero(t, first.Rejected)

		// The host advances its cursor past the accepted event, as the daemon
		// does after an accepting batch, and the server records it.
		next := "after-" + sessionUUID
		w := mnHostHTTP(t, env, http.MethodPost, cursorPath, map[string]any{
			"cursor": next, "base_cursor": base, "cursor_epoch": cur.CursorEpoch,
		})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		readData(t, mnHostHTTP(t, env, http.MethodGet, cursorPath, nil), &cur)
		require.Equal(t, next, cur.Cursor, "the host cursor acknowledges the excluded event")

		again := parseMNIngestResp(t, postMNIngest(t, env, map[string]any{"events": []any{ev}}))
		require.Equal(t, 1, again.Duplicate)
		require.Zero(t, again.Accepted)

		stored, ok := ixnSessionRows(t, env, sessionUUID)["anarlog:"+sessionUUID+":"+env.contactB.String()]
		require.True(t, ok, "the later attendee's raw observation is stored")
		require.Equal(t, node, *stored.InteractionNodeID)
		excludedFrom(t, e, env.contactB, node)
		require.Equal(t, beforeB, cadenceRow(t, env.contactRepo, env.contactB))
	})
}

// mnHostHTTP sends a request as the env's paired Mac host.
func mnHostHTTP(t *testing.T, env *meetingNoteIngestEnv, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", env.pairedHostID.String())
	req.Header.Set("Authorization", "Bearer "+env.pairedHostKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}
