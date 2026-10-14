package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe GET /api/v1/interactions/:id/participants, the read the
// removal dialog lists attendees from.

type participantAnswer struct {
	ContactID uuid.UUID `json:"contact_id"`
	Name      string    `json:"name"`
}

func (e *correctionEnv) getParticipants(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/interactions/"+id+"/participants", nil)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *correctionEnv) participants(t *testing.T, id uuid.UUID) []participantAnswer {
	t.Helper()
	w := e.getParticipants(t, id.String())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Participants []participantAnswer `json:"participants"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	return envelope.Data.Participants
}

func (e *correctionEnv) participantOf(t *testing.T, contactID uuid.UUID) participantAnswer {
	t.Helper()
	c, err := e.contactRepo.GetContact(e.ctx, contactID)
	require.NoError(t, err)
	return participantAnswer{ContactID: contactID, Name: c.FullName}
}

func TestInteractionParticipants(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("lists every effective participant by contact id and display name", func(t *testing.T) {
		// spec: IXN-036.lists-effective-participants
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))

		require.ElementsMatch(t, []participantAnswer{e.participantOf(t, a), e.participantOf(t, b)}, e.participants(t, node))
	})

	t.Run("a removed participant is absent", func(t *testing.T) {
		// spec: IXN-036.removed-absent
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))

		e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, a))

		require.Equal(t, []participantAnswer{e.participantOf(t, b)}, e.participants(t, node))
	})

	t.Run("any id of a merged interaction reads the merged interaction's participants", func(t *testing.T) {
		// spec: IXN-036.merged-reads-merged
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		meeting := e.meeting(t, a, e.event(t), e.at(0))
		call := e.call(t, b, e.ref("call"), repository.InteractionDirectionInbound, e.at(1))
		merged := e.merge(t, a, meeting, call).InteractionID

		want := []participantAnswer{e.participantOf(t, a), e.participantOf(t, b)}
		for _, id := range []uuid.UUID{meeting, call} {
			require.ElementsMatch(t, want, e.participants(t, id), "id %s of merged interaction %s", id, merged)
		}
	})

	t.Run("an id that names no interaction is not found, and a malformed id is invalid", func(t *testing.T) {
		// spec: IXN-036.no-interaction-not-found
		a := e.contact(t, "monthly")
		burst := e.record(t, a, repository.InteractionSourceTelegram, e.ref("tg"), repository.InteractionDirectionInbound, e.at(3))

		for _, id := range []string{burst.ID.String(), a.String(), uuid.NewString()} {
			w := e.getParticipants(t, id)
			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		}
		require.Equal(t, http.StatusBadRequest, e.getParticipants(t, "not-a-uuid").Code)
	})
}
