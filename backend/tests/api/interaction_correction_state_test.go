package api

import (
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	calendarapi "google.golang.org/api/calendar/v3"
)

// These tests observe the correction routes through the assertion store, its
// provenance, the node registry, and the observation and content rows.

// requireUserLocator requires a correction assertion's one locator to carry
// user provenance and, for a removal, the reason as submitted. It returns the
// locator's source id, which names the correction act.
func (e *correctionEnv) requireUserLocator(t *testing.T, a repository.Assertion, reason *string) string {
	t.Helper()
	loc := e.locator(t, a.ID)
	require.Equal(t, repository.SourceKindUser, loc.SourceKind)
	require.Equal(t, repository.ProducerKindUser, loc.ProducerKind)
	if reason == nil {
		require.Nil(t, loc.Field)
		require.Nil(t, loc.Quote)
	} else {
		require.NotNil(t, loc.Field)
		require.Equal(t, "reason", *loc.Field)
		require.NotNil(t, loc.Quote)
		require.Equal(t, *reason, *loc.Quote)
	}
	require.NotEmpty(t, loc.SourceID)
	return loc.SourceID
}

func TestInteractionCorrection_IsSP1State(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	t.Run("a merge aliases the later-listed root to the first without a tombstone and asserts same_interaction_as", func(t *testing.T) {
		// spec: KNW-037
		a := e.contact(t, "monthly")
		nodeA := e.meeting(t, a, e.event(t), e.at(0))
		nodeB := e.meeting(t, a, e.event(t), e.at(1))

		e.merge(t, a, nodeA, nodeB)

		merged, err := e.nodes.GetNode(e.ctx, nodeB)
		require.NoError(t, err)
		require.NotNil(t, merged.MergedInto)
		require.Equal(t, nodeA, *merged.MergedInto)
		require.Nil(t, merged.DeletedAt)
		decl := e.correctionAssertions(t, nodeB, "same_interaction_as")
		require.Len(t, decl, 1)
		require.Equal(t, nodeB, decl[0].SubjectNodeID)
		require.Equal(t, nodeA, *decl[0].ObjectNodeID)
		e.requireUserLocator(t, decl[0], nil)
	})

	t.Run("removing a contact writes one not_participant_in per member node with the reason", func(t *testing.T) {
		// spec: KNW-037
		a := e.contact(t, "monthly")
		nodeX := e.meeting(t, a, e.event(t), e.at(0))
		nodeY := e.call(t, a, e.ref("call"), repository.InteractionDirectionInbound, e.at(1))
		e.merge(t, a, nodeX, nodeY)
		reason := service.RemovalReasonDidNotAttend

		e.remove(t, nodeX, removalBody(reason, a))

		acts := map[string]bool{}
		for _, node := range []uuid.UUID{nodeX, nodeY} {
			removals := e.correctionAssertions(t, node, "not_participant_in")
			require.Len(t, removals, 1)
			require.Equal(t, a, removals[0].SubjectNodeID)
			require.Equal(t, node, *removals[0].ObjectNodeID)
			acts[e.requireUserLocator(t, removals[0], &reason)] = true
		}
		require.Len(t, acts, 1, "the assertions of one act share one locator source id")
	})

	t.Run("removing everyone writes one removed_for_everyone per member node with the reason", func(t *testing.T) {
		// spec: KNW-037
		a := e.contact(t, "monthly")
		nodeX := e.meeting(t, a, e.event(t), e.at(0))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))
		e.merge(t, a, nodeX, nodeY)
		reason := service.RemovalReasonDidNotHappen

		e.remove(t, nodeY, everyoneBody(reason))

		for _, node := range []uuid.UUID{nodeX, nodeY} {
			facts := e.correctionAssertions(t, node, "removed_for_everyone")
			require.Len(t, facts, 1)
			require.Equal(t, node, facts[0].SubjectNodeID)
			require.True(t, *facts[0].ValueBool)
			e.requireUserLocator(t, facts[0], &reason)
		}
	})

	t.Run("two correction acts carry different locator source ids", func(t *testing.T) {
		// spec: KNW-037
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		node := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))
		reason := service.RemovalReasonDuplicate

		e.remove(t, node, removalBody(reason, a))
		e.remove(t, node, removalBody(reason, b))

		removals := e.correctionAssertions(t, node, "not_participant_in")
		require.Len(t, removals, 2)
		require.NotEqual(t, e.requireUserLocator(t, removals[0], &reason), e.requireUserLocator(t, removals[1], &reason))
	})

	t.Run("a later merge leaves every earlier correction and the earlier members as they were", func(t *testing.T) {
		// spec: KNW-037
		a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
		ev := e.event(t)
		nodeX := e.meeting(t, a, ev, e.at(0))
		e.meeting(t, b, ev, e.at(0))
		nodeY := e.meeting(t, a, e.event(t), e.at(1))
		nodeZ := e.meeting(t, a, e.event(t), e.at(2))
		e.merge(t, a, nodeX, nodeY)
		e.remove(t, nodeX, removalBody(service.RemovalReasonDidNotAttend, b))
		earlier := append(e.correctionAssertions(t, nodeY, "same_interaction_as"), e.correctionAssertions(t, nodeX, "not_participant_in")...)
		require.Len(t, earlier, 2)

		e.merge(t, a, nodeZ, nodeX)

		later := append(e.correctionAssertions(t, nodeY, "same_interaction_as"), e.correctionAssertions(t, nodeX, "not_participant_in")...)
		require.Equal(t, earlier, later)
		require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			tree, err := e.interactions.GetInteractionTreeTx(e.ctx, tx, nodeZ)
			require.Equal(t, nodeZ, tree.Root)
			require.ElementsMatch(t, []uuid.UUID{nodeX, nodeY, nodeZ}, tree.Nodes)
			return err
		}))
		y, err := e.nodes.GetNode(e.ctx, nodeY)
		require.NoError(t, err)
		require.Equal(t, nodeX, *y.MergedInto, "the earlier merge's member is still aliased under its winner")
	})

	t.Run("merges and removals change no observation or content row", func(t *testing.T) {
		// spec: KNW-037
		// A database of its own, so the meeting note's linkage window holds the
		// one calendar event this case syncs.
		database, _ := newIsolatedRiverTestDB(t, t.Context())
		e := newCorrectionEnvOn(t, database)
		gen := e.harness.Generator()
		aSpec := gen.Contact(factory.WithCadence("monthly"), factory.WithEmail())
		bSpec := gen.Contact(factory.WithCadence("monthly"), factory.WithEmail())
		aRow, err := e.harness.SeedContact(e.ctx, aSpec)
		require.NoError(t, err)
		bRow, err := e.harness.SeedContact(e.ctx, bSpec)
		require.NoError(t, err)
		a, b := aRow.ID, bRow.ID

		meetingSpec := gen.GCalEvent(aSpec, factory.MatchSeeded)
		meetingSpec.Event.Attendees = append(meetingSpec.Event.Attendees,
			&calendarapi.EventAttendee{Email: bSpec.Email, ResponseStatus: "accepted"})
		_, err = e.harness.ReplayGCal(e.ctx, a, meetingSpec)
		require.NoError(t, err)
		calEvent, err := e.harness.CalendarEventRepo().GetByGcalID(e.ctx, meetingSpec.GcalEventID, "primary", meetingSpec.AccountID)
		require.NoError(t, err)
		meeting, err := e.interactions.FindBySourceRef(e.ctx, a, repository.InteractionSourceGCal, calEvent.ID.String())
		require.NoError(t, err)
		other, err := e.interactions.FindBySourceRef(e.ctx, b, repository.InteractionSourceGCal, calEvent.ID.String())
		require.NoError(t, err)
		title := "Synthetic meeting"
		require.NoError(t, e.harness.ReplayMeetingNoteRecorded(e.ctx, replay.MeetingNoteRecordedSpec{
			SessionID: uuid.New(), Title: &title, MeetingAt: calEvent.StartTime,
		}))

		_, err = e.harness.ReplayGmail(e.ctx, a, gen.GmailMessage(aSpec, factory.MatchSeeded, factory.WithOutbound()))
		require.NoError(t, err)
		email := e.onlyObservation(t, a, repository.InteractionSourceEmail)

		callRef := e.ref("call")
		call := e.record(t, a, repository.InteractionSourcePhoneCalls, callRef, repository.InteractionDirectionInbound, e.at(1))
		e.linkCallRow(t, a, call)

		content := e.contentRows(t, meeting, call, email)
		observations := []*repository.Interaction{meeting, other, call, email}
		rowsBefore := e.observationRows(t, observations)

		e.merge(t, a, *meeting.InteractionNodeID, *call.InteractionNodeID, *email.InteractionNodeID)
		e.remove(t, *call.InteractionNodeID, removalBody(service.RemovalReasonDuplicate, a))
		e.remove(t, *meeting.InteractionNodeID, everyoneBody(service.RemovalReasonDidNotHappen))

		require.Equal(t, rowsBefore, e.observationRows(t, observations))
		require.Equal(t, content, e.contentRows(t, meeting, call, email))
	})
}

// observationRows reads each observation row as stored.
func (e *correctionEnv) observationRows(t *testing.T, observations []*repository.Interaction) []*repository.Interaction {
	t.Helper()
	rows := make([]*repository.Interaction, len(observations))
	for i, o := range observations {
		row, err := e.interactions.GetInteraction(e.ctx, o.ID)
		require.NoError(t, err)
		rows[i] = row
	}
	return rows
}

// linkedContent is every content row linked to the seeded observations.
type linkedContent struct {
	notes []repository.MeetingNote
	calls []repository.PhoneCall
	mails []repository.CommsMessage
}

// onlyObservation returns the contact's one observation from source.
func (e *correctionEnv) onlyObservation(t *testing.T, contactID uuid.UUID, source string) *repository.Interaction {
	t.Helper()
	all, err := e.interactions.ListContactInteractions(e.ctx, contactID, 100, 0)
	require.NoError(t, err)
	var found []repository.Interaction
	for _, ix := range all {
		if ix.Source == source {
			found = append(found, ix)
		}
	}
	require.Len(t, found, 1)
	return &found[0]
}

// linkCallRow stores the phone call row the call observation records. The
// synthetic toolkit has no phone call source, so the row is written through
// the phone call repository's test insert.
func (e *correctionEnv) linkCallRow(t *testing.T, contactID uuid.UUID, call *repository.Interaction) {
	t.Helper()
	_, err := repository.NewPhoneCallRepository(e.database.Queries).TestInsertPhoneCallLinked(e.ctx, repository.TestInsertPhoneCallLinkedParams{
		CallUniqueID:     *call.SourceRef,
		PeerHandle:       "+15550100",
		PeerNormalized:   "+15550100",
		Service:          "voice",
		Direction:        repository.PhoneCallDirectionInbound,
		DurationSeconds:  60,
		StartedAt:        call.OccurredAt,
		MatchedContactID: &contactID,
		InteractionID:    &call.ID,
	})
	require.NoError(t, err)
}

// contentRows reads the content rows linked to the observations: the meeting
// note linked to the meeting's calendar event, the call row and the message.
func (e *correctionEnv) contentRows(t *testing.T, meeting, call, email *repository.Interaction) linkedContent {
	t.Helper()
	eventID, err := uuid.Parse(*meeting.SourceRef)
	require.NoError(t, err)
	var c linkedContent
	c.notes, err = repository.NewMeetingNoteRepository(e.database.Queries).ListByLinkedRefs(e.ctx, repository.LinkedKindEvent, []uuid.UUID{eventID})
	require.NoError(t, err)
	require.Len(t, c.notes, 1)
	c.calls, err = repository.NewPhoneCallRepository(e.database.Queries).ListByInteractionIDs(e.ctx, []uuid.UUID{call.ID})
	require.NoError(t, err)
	require.Len(t, c.calls, 1)
	c.mails, err = repository.NewCommsMessageRepository(e.database.Queries).ListByInteractionIDs(e.ctx, []uuid.UUID{email.ID})
	require.NoError(t, err)
	require.Len(t, c.mails, 1)
	return c
}
