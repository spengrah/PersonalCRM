//go:build integration_testdb

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// These tests own a database clone, so they can count every
// interaction.corrected event and every River job.

// correctionState is everything a correction could write, for the nodes and
// contacts a test names.
type correctionState struct {
	aliases    map[uuid.UUID]*uuid.UUID
	assertions map[uuid.UUID]int
	fields     map[uuid.UUID]contactFields
	tasks      map[uuid.UUID][]repository.ContactTask
	events     int64
	jobs       int
}

func (e *correctionEnv) snapshot(t *testing.T, nodes, contacts []uuid.UUID) correctionState {
	t.Helper()
	s := correctionState{
		aliases:    map[uuid.UUID]*uuid.UUID{},
		assertions: map[uuid.UUID]int{},
		fields:     map[uuid.UUID]contactFields{},
		tasks:      map[uuid.UUID][]repository.ContactTask{},
		events:     e.correctionEventCount(t),
		jobs:       len(e.jobs(t)),
	}
	for _, n := range nodes {
		node, err := e.nodes.GetNode(e.ctx, n)
		require.NoError(t, err)
		s.aliases[n] = node.MergedInto
		for _, p := range []string{"same_interaction_as", "not_participant_in", "removed_for_everyone"} {
			s.assertions[n] += len(e.correctionAssertions(t, n, p))
		}
	}
	for _, c := range contacts {
		s.fields[c] = e.fields(t, c)
		tasks, err := e.tasks.ListContactTasksByContact(e.ctx, c)
		require.NoError(t, err)
		s.tasks[c] = tasks
	}
	return s
}

// jobs lists every River job in the env's database.
func (e *correctionEnv) jobs(t *testing.T) []*rivertype.JobRow {
	t.Helper()
	res, err := e.riverClient.JobList(e.ctx, river.NewJobListParams().First(10000))
	require.NoError(t, err)
	return res.Jobs
}

func TestInteractionCorrection_Refusals(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	database, _ := newIsolatedRiverTestDB(t, t.Context())
	e := newCorrectionEnvOn(t, database)

	a, b, stranger := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
	ev := e.event(t)
	nodeX := e.meeting(t, a, ev, e.at(0))
	e.meeting(t, b, ev, e.at(0))
	nodeY := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(1))
	nodeZ := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(2))
	e.merge(t, a, nodeY, nodeZ)
	burst := e.record(t, a, repository.InteractionSourceTelegram, e.ref("tg"), repository.InteractionDirectionInbound, e.at(3))
	nodes := []uuid.UUID{nodeX, nodeY, nodeZ}
	contacts := []uuid.UUID{a, b, stranger}

	// refuse posts body to path, requires the refusal status inside the error
	// envelope, and requires that nothing changed.
	refuse := func(t *testing.T, path string, body any, status int) {
		t.Helper()
		before := e.snapshot(t, nodes, contacts)
		w := e.post(t, path, body)
		require.Equal(t, status, w.Code, w.Body.String())
		var envelope struct {
			Success bool `json:"success"`
			Error   *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.False(t, envelope.Success)
		require.NotNil(t, envelope.Error)
		require.Equal(t, before, e.snapshot(t, nodes, contacts), "a refusal writes nothing and publishes no event")
	}
	removals := func(id uuid.UUID) string { return "/api/v1/interactions/" + id.String() + "/removals" }
	const merge, preview = "/api/v1/interactions/merge", "/api/v1/interactions/merge/preview"

	t.Run("a merge or preview resolving to fewer than two interactions answers 400", func(t *testing.T) {
		// spec: IXN-029.fewer-than-two-refused, IXN-029.refusal-writes-nothing
		refuse(t, merge, mergeBody(a, nodeX), http.StatusBadRequest)
		refuse(t, merge, mergeBody(a, nodeY, nodeZ), http.StatusBadRequest)
		refuse(t, preview, mergeBody(a, nodeZ, nodeY), http.StatusBadRequest)
	})

	t.Run("a merge or preview id naming no interaction node answers 404", func(t *testing.T) {
		// spec: IXN-029.unknown-interaction-not-found, IXN-029.refusal-writes-nothing
		refuse(t, merge, mergeBody(a, nodeX, uuid.New()), http.StatusNotFound)
		refuse(t, preview, mergeBody(a, nodeX, burst.ID), http.StatusNotFound)
		refuse(t, merge, mergeBody(a, nodeX, b), http.StatusNotFound)
	})

	t.Run("a contact_id naming no live contact answers 404, and one with no participation answers 400", func(t *testing.T) {
		// spec: IXN-029.contact-refused, IXN-029.refusal-writes-nothing
		refuse(t, merge, mergeBody(uuid.New(), nodeX, nodeY), http.StatusNotFound)
		refuse(t, merge, mergeBody(stranger, nodeX, nodeY), http.StatusBadRequest)
		refuse(t, preview, mergeBody(stranger, nodeX, nodeY), http.StatusBadRequest)
	})

	t.Run("a removal with neither or both scopes, an empty contact list or an unknown reason answers 400", func(t *testing.T) {
		// spec: IXN-029.removal-body-refused, IXN-029.refusal-writes-nothing
		refuse(t, removals(nodeX), map[string]any{"scope": map[string]any{}, "reason": service.RemovalReasonDuplicate}, http.StatusBadRequest)
		refuse(t, removals(nodeX), map[string]any{"scope": map[string]any{"contact_ids": []string{a.String()}, "everyone": true},
			"reason": service.RemovalReasonDuplicate}, http.StatusBadRequest)
		refuse(t, removals(nodeX), map[string]any{"scope": map[string]any{"contact_ids": []string{}}, "reason": service.RemovalReasonDuplicate},
			http.StatusBadRequest)
		refuse(t, removals(nodeX), map[string]any{"scope": map[string]any{"contact_ids": []string{}, "everyone": true},
			"reason": service.RemovalReasonDuplicate}, http.StatusBadRequest)
		refuse(t, removals(nodeX), map[string]any{"scope": map[string]any{"everyone": false}, "reason": service.RemovalReasonDuplicate},
			http.StatusBadRequest)
		refuse(t, removals(nodeX), removalBody("no_reason", a), http.StatusBadRequest)
	})

	t.Run("a removal whose id names no interaction node answers 404", func(t *testing.T) {
		// spec: IXN-029.removal-unknown-interaction-not-found, IXN-029.refusal-writes-nothing
		refuse(t, removals(uuid.New()), removalBody(service.RemovalReasonDuplicate, a), http.StatusNotFound)
		refuse(t, removals(burst.ID), everyoneBody(service.RemovalReasonDuplicate), http.StatusNotFound)
	})

	t.Run("a removal listing a contact that is not an effective participant answers 400", func(t *testing.T) {
		// spec: IXN-029.non-participant-refused, IXN-029.refusal-writes-nothing
		refuse(t, removals(nodeX), removalBody(service.RemovalReasonDuplicate, a, stranger), http.StatusBadRequest)
	})

	t.Run("a removal of an interaction with no effective participant answers 400", func(t *testing.T) {
		// spec: IXN-029.no-participant-refused, IXN-029.refusal-writes-nothing
		ev := e.event(t)
		gone := e.meeting(t, a, ev, e.at(4))
		e.meeting(t, b, ev, e.at(4))
		e.remove(t, gone, everyoneBody(service.RemovalReasonDidNotHappen))
		nodes = append(nodes, gone)
		refuse(t, removals(gone), everyoneBody(service.RemovalReasonDidNotHappen), http.StatusBadRequest)
	})
}

func TestInteractionCorrection_PublishesOneEvent(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	database, _ := newIsolatedRiverTestDB(t, t.Context())
	e := newCorrectionEnvOn(t, database)

	// actOf returns the act id a correction assertion's locator names.
	actOf := func(t *testing.T, node uuid.UUID, predicate string) string {
		t.Helper()
		as := e.correctionAssertions(t, node, predicate)
		require.NotEmpty(t, as)
		return e.locator(t, as[0].ID).SourceID
	}
	// requireNoConsumerJob requires that no River job names the event.
	requireNoConsumerJob := func(t *testing.T, act string) {
		t.Helper()
		env, err := e.events.FindEventBySource(e.ctx, "interaction_correction", act)
		require.NoError(t, err)
		for _, job := range e.jobs(t) {
			var args struct {
				EventID uuid.UUID `json:"event_id"`
			}
			require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
			require.NotEqual(t, env.ID, args.EventID, "job %s names the interaction.corrected event", job.Kind)
		}
	}

	t.Run("a merge publishes one event naming merge, the winner and every recomputed contact", func(t *testing.T) {
		// spec: IXN-029.merge-answers-participation
		a, b, outsider := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
		evX, evY := e.event(t), e.event(t)
		nodeX := e.meeting(t, a, evX, e.at(0))
		e.meeting(t, b, evX, e.at(0))
		nodeY := e.meeting(t, b, evY, e.at(1))
		e.meeting(t, outsider, e.event(t), e.at(1))
		before := e.correctionEventCount(t)

		w := e.post(t, "/api/v1/interactions/merge", mergeBody(a, nodeY, nodeX))
		require.Equal(t, http.StatusOK, w.Code)
		var envelope struct {
			Success bool           `json:"success"`
			Data    map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.True(t, envelope.Success)
		require.Equal(t, nodeY.String(), envelope.Data["interaction_id"])
		require.Equal(t, map[string]any{"direction": "mutual", "occurred_at": e.at(0).Format("2006-01-02T15:04:05Z07:00")}, envelope.Data["participation"])

		require.Equal(t, before+1, e.correctionEventCount(t))
		act := actOf(t, nodeX, "same_interaction_as")
		payload := e.correctionEvent(t, act)
		require.Equal(t, events.CorrectionMerge, payload.Correction)
		require.Equal(t, nodeY, payload.InteractionNodeID)
		require.ElementsMatch(t, []uuid.UUID{a, b}, payload.RecomputedContactIDs)
		requireNoConsumerJob(t, act)
	})

	t.Run("a merge recomputes an attendee observed on a member and removed from it", func(t *testing.T) {
		// spec: CAD-047.only-affected-contacts-recomputed
		a, removed := e.contact(t, "monthly"), e.contact(t, "monthly")
		evX := e.event(t)
		nodeX := e.meeting(t, a, evX, e.at(0))
		e.meeting(t, removed, evX, e.at(0))
		e.remove(t, nodeX, removalBody(service.RemovalReasonDidNotAttend, removed))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))

		e.merge(t, a, nodeY, nodeX)

		payload := e.correctionEvent(t, actOf(t, nodeX, "same_interaction_as"))
		require.ElementsMatch(t, []uuid.UUID{a, removed}, payload.RecomputedContactIDs)
	})

	t.Run("a removal publishes one event naming removal, the root and every recomputed contact", func(t *testing.T) {
		// spec: IXN-029.removal-answers-no-content
		a, b, c := e.contact(t, "monthly"), e.contact(t, "monthly"), e.contact(t, "monthly")
		evX, evY := e.event(t), e.event(t)
		nodeX := e.meeting(t, a, evX, e.at(0))
		e.meeting(t, b, evX, e.at(0))
		e.meeting(t, c, evX, e.at(0))
		nodeY := e.meeting(t, a, evY, e.at(1))
		e.merge(t, a, nodeX, nodeY)
		before := e.correctionEventCount(t)

		e.remove(t, nodeY, removalBody(service.RemovalReasonDidNotAttend, a, c))

		require.Equal(t, before+1, e.correctionEventCount(t))
		act := actOf(t, nodeX, "not_participant_in")
		payload := e.correctionEvent(t, act)
		require.Equal(t, events.CorrectionRemoval, payload.Correction)
		require.Equal(t, nodeX, payload.InteractionNodeID)
		require.ElementsMatch(t, []uuid.UUID{a, c}, payload.RecomputedContactIDs)
		requireNoConsumerJob(t, act)
	})

	t.Run("a preview writes no alias, assertion, cadence change, task change or event", func(t *testing.T) {
		// spec: IXN-017.preview-writes-nothing
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		nodeX := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))
		nodeY := e.call(t, a, e.ref("call"), repository.InteractionDirectionOutbound, e.at(2))
		before := e.snapshot(t, []uuid.UUID{nodeX, nodeY}, []uuid.UUID{a, b})
		require.NotEmpty(t, before.tasks[a], "the outbound opened a follow-up a merge would close")

		e.preview(t, a, nodeY, nodeX)

		require.Equal(t, before, e.snapshot(t, []uuid.UUID{nodeX, nodeY}, []uuid.UUID{a, b}))
	})
}
