//go:build integration_testdb

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// attend records a contact's attendance of calendar event ref (see event)
// through the interaction recorder, the bus path a calendar sync takes, in its
// own transaction. It returns the recorded interaction.
func (e *correctionEnv) attend(contactID uuid.UUID, ref string, at time.Time) (*repository.Interaction, error) {
	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: contactID, EventID: ref, OccurredAt: at,
	})
	if err != nil {
		return nil, err
	}
	env := &events.Envelope{ID: uuid.New(), Source: repository.InteractionSourceGCal, SourceID: uuid.NewString(),
		Kind: events.KindCalendarAttended, Payload: payload, ObservedAt: at}
	tx, err := e.database.Pool.Begin(e.ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	ix, err := e.recorder.HandleEvent(e.ctx, tx, env)
	if err != nil {
		return nil, err
	}
	return ix, tx.Commit(e.ctx)
}

// TestInteractionCorrection_SerializedWithRecording runs a removal and a
// cadence-moving write of the same contact concurrently: both queue on the
// contact's row while another transaction holds it, the removal first. Once
// both commit, the contact's fields are those of a twin on which the removed
// observation was never recorded and the other write was made.
func TestInteractionCorrection_SerializedWithRecording(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)
	day := 24

	t.Run("a calendar recording queued behind a removal decides its cadence effect from the corrected state", func(t *testing.T) {
		// spec: CAD-047.recomputed-from-effective-participation
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*day))
		removed := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-10*day))
		e.call(t, twin, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*day))
		twinIx, err := e.attend(twin, e.event(t), e.at(-30*day))
		require.NoError(t, err)
		e.finalizeRecorded(t, repository.InteractionSourceGCal, twinIx.ID)
		ref := e.event(t)

		var ix *repository.Interaction
		e.raceRemoval(t, a, removed, func() (err error) {
			ix, err = e.attend(a, ref, e.at(-30*day))
			return err
		})
		e.finalizeRecorded(t, repository.InteractionSourceGCal, ix.ID)

		require.Equal(t, e.fields(t, twin), e.fields(t, a))
	})

	t.Run("an extension queued behind a removal decides its cadence effect from the corrected state", func(t *testing.T) {
		// spec: CAD-047.recomputed-from-effective-participation
		a, twin := e.contact(t, "monthly"), e.contact(t, "monthly")
		e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*day))
		extended := e.record(t, a, repository.InteractionSourcePhoneCalls, e.ref("call"), repository.InteractionDirectionInbound, e.at(-35*day))
		removed := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(-10*day))
		e.call(t, twin, e.ref("call"), repository.InteractionDirectionInbound, e.at(-40*day))
		twinExtended := e.record(t, twin, repository.InteractionSourcePhoneCalls, e.ref("call"), repository.InteractionDirectionInbound, e.at(-35*day))
		require.NoError(t, e.contacts.ExtendInteraction(e.ctx, twinExtended.ID, twin, repository.InteractionDirectionInbound, e.at(-30*day), nil))

		e.raceRemoval(t, a, removed, func() error {
			return e.contacts.ExtendInteraction(e.ctx, extended.ID, a, repository.InteractionDirectionInbound, e.at(-30*day), nil)
		})

		require.Equal(t, e.fields(t, twin), e.fields(t, a))
	})
}

// raceRemoval holds contactID's row, queues the removal of the contact from the
// interaction holding node on it, then queues write on it behind the removal,
// and releases the row. It requires the removal to answer 204 and write to
// succeed.
func (e *correctionEnv) raceRemoval(t *testing.T, contactID, node uuid.UUID, write func() error) {
	t.Helper()
	holder, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(e.ctx) }()
	require.NoError(t, e.contactRepo.LockContactForDateRecomputeTx(e.ctx, holder, contactID))
	holderPID, err := db.New(holder).TestBackendPID(e.ctx)
	require.NoError(t, err)
	waiting := func(n int64) func() bool {
		return func() bool {
			got, err := e.database.Queries.TestCountBackendsWaitingOn(e.ctx, holderPID)
			return err == nil && got == n
		}
	}

	removal := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		raw, _ := json.Marshal(removalBody(service.RemovalReasonDidNotHappen, contactID))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/interactions/"+node.String()+"/removals", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		removal <- w
	}()
	require.Eventually(t, waiting(1), 10*time.Second, 10*time.Millisecond, "the removal never queued on the contact's row")

	written := make(chan error, 1)
	go func() { written <- write() }()
	require.Eventually(t, waiting(2), 10*time.Second, 10*time.Millisecond, "the write never queued on the contact's row")

	require.NoError(t, holder.Commit(e.ctx))
	w := <-removal
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.NoError(t, <-written)
}
