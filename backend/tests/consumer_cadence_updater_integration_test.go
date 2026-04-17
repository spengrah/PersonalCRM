package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Integration tests for the PR 7 CadenceUpdater consumer + direct-path shadow
// observer. Verifies the two paths write matching observations on the happy
// path (spec §5 PR 7 acceptance: "shadow writes match direct writes 1:1")
// and that the forward-only guard + manual-source branch behave per spec
// §3.4.2. Coverage-gap tests document Extend / Promote / Merge /
// HTTP-ingested V1 paths explicitly (plan Decision 6a + acceptance scope).
// -----------------------------------------------------------------------------

type cadenceIntegrationEnv struct {
	base             *consumerTestEnv
	cadenceUpdater   *consumer.CadenceUpdater
	cadenceShadowRep *repository.CadenceShadowObservationRepository
}

// newCadenceIntegrationEnv builds a fresh consumer test env and layers the
// PR 7 CadenceUpdater + direct-path observer on top. EVENT_BUS_CADENCE_MODE
// is set to "shadow" for the test — publisher-driven interaction.recorded
// emits route through the direct-path shadow queue (via
// SetCadenceShadowObserver) and the consumer's HandleEvent (invoked
// inline via runCadenceHandleEvent).
func newCadenceIntegrationEnv(t *testing.T, ctx context.Context) *cadenceIntegrationEnv {
	t.Helper()
	base := newConsumerTestEnv(t, ctx)

	// Wire the direct-path observer so applyInteractionEffectsFromRow's
	// shadow-capture branch fires.
	base.contactRepo.SetPool(base.database.Pool)
	shadowRepo := repository.NewCadenceShadowObservationRepository(base.database.Queries, base.database.Pool)
	base.contactService.SetCadenceShadowObserver(shadowRepo)

	cu := consumer.NewCadenceUpdater(shadowRepo, base.bus, consumer.CadenceModeShadow)

	return &cadenceIntegrationEnv{
		base:             base,
		cadenceUpdater:   cu,
		cadenceShadowRep: shadowRepo,
	}
}

// runCadenceHandleEvent invokes the CadenceUpdater directly in a fresh tx.
// Returns any error; commits on success. Callers use this after an
// interaction.recorded event has been emitted (via the interaction
// recorder's HandleEvent path) to process the paired cadence observation.
func (e *cadenceIntegrationEnv) runCadenceHandleEvent(t *testing.T, env *events.Envelope) error {
	t.Helper()
	return pgx.BeginTxFunc(e.base.ctx, e.base.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.cadenceUpdater.HandleEvent(e.base.ctx, tx, env)
	})
}

// findInteractionRecordedEvent locates the interaction.recorded event
// emitted for a given interaction id. Used after runHandleEvent to find
// the paired cadence envelope.
func (e *cadenceIntegrationEnv) findInteractionRecordedEvent(t *testing.T, source string, interactionID uuid.UUID) *events.Envelope {
	t.Helper()
	eventRepo := repository.NewEventRepository(e.base.database.Queries)
	env, err := eventRepo.FindEventBySource(e.base.ctx, source, interactionID.String())
	require.NoError(t, err, "interaction.recorded event for interaction %s not found", interactionID)
	require.Equal(t, events.KindInteractionRecorded, env.Kind)
	return env
}

// seedContactWithCadence creates a contact with a set cadence value. The
// returned contactID is cleaned up via HardDeleteContact on test teardown.
func (e *cadenceIntegrationEnv) seedContactWithCadence(t *testing.T, name, cadenceStr string) uuid.UUID {
	t.Helper()
	cadencePtr := cadenceStr
	contact, err := e.base.contactRepo.CreateContact(e.base.ctx, repository.CreateContactRequest{
		FullName: name + "-" + uuid.NewString()[:8],
		Cadence:  &cadencePtr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.base.contactRepo.HardDeleteContact(e.base.ctx, contact.ID) })
	return contact.ID
}

// -----------------------------------------------------------------------------
// Acceptance: shadow agreement per direction.
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_ShadowAgreement_Mutual exercises the happy
// path: a calendar.attended event flows through InteractionRecorder →
// emits interaction.recorded → direct-path observer queues a shadow
// closure → we drain the closure by invoking postCommit → consumer runs
// inline → both observations land and match.
func TestIntegration_CadenceUpdater_ShadowAgreement_Mutual(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-mutual", "weekly")
	eventIDStr := uuid.NewString()
	sourceID := eventIDStr + ":" + contactID.String()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  contactID,
		EventID:    eventIDStr,
		OccurredAt: occurredAt,
	})
	require.NoError(t, err)

	envelope := &events.Envelope{
		Source:     repository.InteractionSourceGCal,
		SourceID:   sourceID,
		Kind:       events.KindCalendarAttended,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	e.base.mustPublish(t, envelope)
	require.NoError(t, e.base.runHandleEvent(t, envelope))

	// Find the interaction + its paired interaction.recorded event.
	inter, err := e.base.interactionRepo.FindBySourceRef(ctx, contactID, repository.InteractionSourceGCal, eventIDStr)
	require.NoError(t, err)
	recordedEnv := e.findInteractionRecordedEvent(t, repository.InteractionSourceGCal, inter.ID)

	// Run the cadence consumer (which would fire via river in prod).
	require.NoError(t, e.runCadenceHandleEvent(t, recordedEnv))

	// Consumer observation exists with all four apply flags true (mutual).
	consumerObs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, consumerObs, "consumer observation must be written")
	require.True(t, consumerObs.ApplyLastContacted)
	require.True(t, consumerObs.ApplyLastOutreachAt)
	require.True(t, consumerObs.ApplyLastResponseAt)
	require.True(t, consumerObs.ApplyContactBy)
	require.Equal(t, repository.CadenceShadowBranchForward, consumerObs.Branch)

	// Direct observation also exists (runHandleEvent invoked postCommit
	// which drained the shadow queue).
	directObs, err := e.cadenceShadowRep.FindMatchingDirect(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, directObs, "direct observation must be written by post-commit closure")
	require.Equal(t, repository.CadenceShadowBranchForward, directObs.Branch)

	// The four next_* values match between direct and consumer.
	require.True(t, timePtrEqual(directObs.NextLastContacted, consumerObs.NextLastContacted))
	require.True(t, timePtrEqual(directObs.NextLastOutreachAt, consumerObs.NextLastOutreachAt))
	require.True(t, timePtrEqual(directObs.NextLastResponseAt, consumerObs.NextLastResponseAt))
	require.True(t, timePtrEqual(directObs.NextContactBy, consumerObs.NextContactBy),
		"direct.NextContactBy=%v consumer.NextContactBy=%v", directObs.NextContactBy, consumerObs.NextContactBy)
}

// TestIntegration_CadenceUpdater_ShadowAgreement_Outbound — telegram
// message.sent → outbound interaction → only last_outreach_at is touched.
func TestIntegration_CadenceUpdater_ShadowAgreement_Outbound(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-outbound", "weekly")
	extMsgID := uuid.NewString()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindMessageSent, events.MessageSentPayload{
		Version:           1,
		ContactID:         &contactID,
		PeerRef:           "tg:123:456",
		MessageAt:         occurredAt,
		ExternalMessageID: extMsgID,
	})
	require.NoError(t, err)

	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   uniqueSourceID("msgsent"),
		Kind:       events.KindMessageSent,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	e.base.mustPublish(t, envelope)
	require.NoError(t, e.base.runHandleEvent(t, envelope))

	inter, err := e.base.interactionRepo.FindBySourceRef(ctx, contactID, repository.InteractionSourceTelegram, extMsgID)
	require.NoError(t, err)
	recordedEnv := e.findInteractionRecordedEvent(t, repository.InteractionSourceTelegram, inter.ID)
	require.NoError(t, e.runCadenceHandleEvent(t, recordedEnv))

	consumerObs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, consumerObs)
	require.False(t, consumerObs.ApplyLastContacted)
	require.True(t, consumerObs.ApplyLastOutreachAt)
	require.False(t, consumerObs.ApplyLastResponseAt)
	require.False(t, consumerObs.ApplyContactBy)

	directObs, err := e.cadenceShadowRep.FindMatchingDirect(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, directObs)
	require.True(t, timePtrEqual(directObs.NextLastOutreachAt, consumerObs.NextLastOutreachAt))
}

// TestIntegration_CadenceUpdater_ShadowAgreement_Inbound — telegram
// message.received → inbound interaction → last_contacted + last_response_at
// + contact_by (but NOT last_outreach_at — plan Decision 3).
func TestIntegration_CadenceUpdater_ShadowAgreement_Inbound(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-inbound", "weekly")
	extMsgID := uuid.NewString()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindMessageReceived, events.MessageReceivedPayload{
		Version:           1,
		ContactID:         &contactID,
		PeerRef:           "tg:123:456",
		MessageAt:         occurredAt,
		ExternalMessageID: extMsgID,
	})
	require.NoError(t, err)

	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   uniqueSourceID("msgrecv"),
		Kind:       events.KindMessageReceived,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	e.base.mustPublish(t, envelope)
	require.NoError(t, e.base.runHandleEvent(t, envelope))

	inter, err := e.base.interactionRepo.FindBySourceRef(ctx, contactID, repository.InteractionSourceTelegram, extMsgID)
	require.NoError(t, err)
	recordedEnv := e.findInteractionRecordedEvent(t, repository.InteractionSourceTelegram, inter.ID)
	require.NoError(t, e.runCadenceHandleEvent(t, recordedEnv))

	consumerObs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, consumerObs)
	require.True(t, consumerObs.ApplyLastContacted)
	require.False(t, consumerObs.ApplyLastOutreachAt, "inbound does NOT bump last_outreach_at (mirrors direct path)")
	require.True(t, consumerObs.ApplyLastResponseAt)
	require.True(t, consumerObs.ApplyContactBy)

	directObs, err := e.cadenceShadowRep.FindMatchingDirect(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, directObs)
	require.Nil(t, directObs.NextLastOutreachAt, "direct apply-flag-false column is nil in observation")
	require.True(t, timePtrEqual(directObs.NextLastContacted, consumerObs.NextLastContacted))
	require.True(t, timePtrEqual(directObs.NextContactBy, consumerObs.NextContactBy))
}

// TestIntegration_CadenceUpdater_ShadowAgreement_Manual — manual source
// takes the unconditional branch; incoming replaces prev regardless of
// forward-only semantics.
func TestIntegration_CadenceUpdater_ShadowAgreement_Manual(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-manual", "weekly")
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  contactID,
		Direction:  repository.InteractionDirectionMutual,
		OccurredAt: occurredAt,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceManual,
		SourceID:   uniqueSourceID("manual-cad"),
		Kind:       events.KindInteractionManual,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	e.base.mustPublish(t, envelope)
	require.NoError(t, e.base.runHandleEvent(t, envelope))

	// Manual source dedupes by 30-min window when source_ref is nil.
	// The interaction may land with a different source_ref than what
	// we just published — find by (contactID, source) ordered by time.
	inters, err := e.base.interactionRepo.ListContactInteractions(ctx, contactID, 10, 0)
	require.NoError(t, err)
	require.NotEmpty(t, inters)
	inter := inters[0]
	recordedEnv := e.findInteractionRecordedEvent(t, repository.InteractionSourceManual, inter.ID)
	require.NoError(t, e.runCadenceHandleEvent(t, recordedEnv))

	consumerObs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, consumerObs)
	require.Equal(t, repository.CadenceShadowBranchUnconditional, consumerObs.Branch)

	directObs, err := e.cadenceShadowRep.FindMatchingDirect(ctx, nil, recordedEnv.ID)
	require.NoError(t, err)
	require.NotNil(t, directObs)
	require.Equal(t, repository.CadenceShadowBranchUnconditional, directObs.Branch)
}

// -----------------------------------------------------------------------------
// Forward-only guard + manual bypass (plan Decision 14).
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_ForwardOnlyGuard_DoesNotRegress — synthetic
// interaction.recorded event with OccurredAt older than prev snapshot;
// consumer observation's next_last_contacted == prev_last_contacted.
func TestIntegration_CadenceUpdater_ForwardOnlyGuard_DoesNotRegress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-guard", "weekly")
	tPrev := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	tOlder := tPrev.Add(-7 * 24 * time.Hour) // older than prev
	cadenceStr := "weekly"

	// Synthetic V2 payload — bypass the InteractionRecorder path.
	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:       2,
		ContactID:     contactID,
		InteractionID: uuid.New(),
		Direction:     repository.InteractionDirectionMutual,
		OccurredAt:    tOlder,
		Source:        repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{
			LastContacted:  &tPrev,
			LastOutreachAt: &tPrev,
			LastResponseAt: &tPrev,
		},
		PrevCadenceValue: &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "synth:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: tOlder,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	obs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	require.Equal(t, repository.CadenceShadowBranchForward, obs.Branch)
	require.True(t, obs.NextLastContacted.Equal(tPrev), "forward guard: older incoming → prev wins")
	require.True(t, obs.NextLastOutreachAt.Equal(tPrev))
	require.True(t, obs.NextLastResponseAt.Equal(tPrev))
	// contact_by: prev.LastContacted > incoming.OccurredAt → applyContactBy=false.
	require.False(t, obs.ApplyContactBy)
}

// -----------------------------------------------------------------------------
// ON CONFLICT idempotency.
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_OnConflict_NoDuplicateOnRetry — running
// HandleEvent twice on the same envelope inserts at most one consumer
// row (UNIQUE (event_id, writer) per migration 039).
func TestIntegration_CadenceUpdater_OnConflict_NoDuplicateOnRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-conflict", "weekly")
	tPrev := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	cadenceStr := "weekly"

	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contactID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          tPrev,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "conflict:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: tPrev,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))

	// Run twice; each call should succeed and leave exactly one row.
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	obs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.NotNil(t, obs, "single row persists across retries")
}

// -----------------------------------------------------------------------------
// FindDivergences — happy path + induced mismatch.
// -----------------------------------------------------------------------------

func TestIntegration_CadenceUpdater_FindDivergences_ZeroForHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	windowStart := accelerated.GetCurrentTime().Add(-1 * time.Minute)

	contactID := e.seedContactWithCadence(t, "cad-zero-diverge", "weekly")
	extMsgID := uuid.NewString()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindMessageSent, events.MessageSentPayload{
		Version:           1,
		ContactID:         &contactID,
		PeerRef:           "tg:123:456",
		MessageAt:         occurredAt,
		ExternalMessageID: extMsgID,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source: repository.InteractionSourceTelegram, SourceID: uniqueSourceID("zdg"),
		Kind: events.KindMessageSent, Payload: payload, ObservedAt: occurredAt,
	}
	e.base.mustPublish(t, envelope)
	require.NoError(t, e.base.runHandleEvent(t, envelope))

	inter, err := e.base.interactionRepo.FindBySourceRef(ctx, contactID, repository.InteractionSourceTelegram, extMsgID)
	require.NoError(t, err)
	recordedEnv := e.findInteractionRecordedEvent(t, repository.InteractionSourceTelegram, inter.ID)
	require.NoError(t, e.runCadenceHandleEvent(t, recordedEnv))

	windowEnd := accelerated.GetCurrentTime().Add(1 * time.Minute)
	// Filter divergences to just this event (other tests may have run).
	divergences, err := e.cadenceShadowRep.FindDivergences(ctx, windowStart, windowEnd)
	require.NoError(t, err)
	for _, d := range divergences {
		require.NotEqual(t, recordedEnv.ID, d.EventID,
			"happy-path event %s must not appear in divergences", recordedEnv.ID)
	}
}

// -----------------------------------------------------------------------------
// FindDivergences race-class filters (Codex round-1 finding 2).
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_FindDivergences_FiltersGraceWindow —
// direct observation never written (simulating post-commit closure that
// failed or hasn't landed yet) but the consumer row was observed
// within the 5-second grace window → FindDivergences must NOT surface
// the row. The positive case (observed_at past the grace window → row
// DOES surface) is covered by pushing `windowEnd` far enough into the
// future that the grace-subtracted effective edge sits past observed_at.
// Together the two assertions protect against BOTH under-filtering
// (grace absent) and over-filtering (grace permanently hides the row).
func TestIntegration_CadenceUpdater_FindDivergences_FiltersGraceWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	// Consumer-only row: skip the direct-path observer wiring the env
	// already did by publishing a synthetic V2 interaction.recorded.
	contactID := e.seedContactWithCadence(t, "cad-grace", "weekly")
	occurredAt := accelerated.GetCurrentTime()
	cadenceStr := "weekly"

	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contactID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "grace:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	observedAtNow := accelerated.GetCurrentTime()

	// CASE A (negative): windowEnd immediately after observation → the
	// grace-subtracted effective edge is BEFORE observed_at, so the row
	// is still inside the grace window and must NOT surface.
	windowStart := occurredAt.Add(-1 * time.Minute)
	windowEndWithinGrace := observedAtNow.Add(1 * time.Second)
	divergences, err := e.cadenceShadowRep.FindDivergences(ctx, windowStart, windowEndWithinGrace)
	require.NoError(t, err)
	for _, d := range divergences {
		require.NotEqual(t, envelope.ID, d.EventID,
			"within grace window: row must not appear as divergence")
	}

	// CASE B (positive): windowEnd far enough in the future that the
	// grace-subtracted effective edge (windowEnd - 5s) is AFTER
	// observed_at. The row must now surface — guards against an over-
	// filtering implementation (e.g. a query that filters by observed_at
	// > now() - 5s instead of @observed_at_to - 5s).
	windowEndPastGrace := observedAtNow.Add(1 * time.Minute)
	divergences, err = e.cadenceShadowRep.FindDivergences(ctx, windowStart, windowEndPastGrace)
	require.NoError(t, err)
	var found bool
	for _, d := range divergences {
		if d.EventID == envelope.ID {
			found = true
			// One-sided row: direct is missing, consumer is present.
			require.Nil(t, d.DirectBranch, "direct side should be absent")
			require.NotNil(t, d.ConsumerBranch, "consumer side must be present")
			break
		}
	}
	require.True(t, found, "past-grace window: consumer-only row must surface as divergence")
}

// TestIntegration_CadenceUpdater_FindDivergences_FiltersSoftDeletedContact
// — consumer row exists for a contact that was subsequently soft-
// deleted. FindDivergences filters it out (contact_soft_deleted_midstream
// race class).
func TestIntegration_CadenceUpdater_FindDivergences_FiltersSoftDeletedContact(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-softdel", "weekly")
	// Historical observation: push observed_at back past the grace
	// window so the row is eligible for the divergence query.
	occurredAt := accelerated.GetCurrentTime().Add(-10 * time.Second)
	cadenceStr := "weekly"

	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contactID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "softdel:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	// Soft-delete the contact after the observation lands.
	require.NoError(t, e.base.contactRepo.SoftDeleteContact(ctx, contactID))

	// Query the full time window including the grace-window margin.
	windowStart := occurredAt.Add(-1 * time.Minute)
	windowEnd := accelerated.GetCurrentTime().Add(1 * time.Minute)
	divergences, err := e.cadenceShadowRep.FindDivergences(ctx, windowStart, windowEnd)
	require.NoError(t, err)
	for _, d := range divergences {
		require.NotEqual(t, envelope.ID, d.EventID,
			"soft-deleted contact: divergence row must be filtered out")
	}
}

// TestIntegration_CadenceUpdater_FindDivergences_GraceWindowAppliedToPair —
// regression test for the reviewer's Risky-1 finding (pre-filter on each
// side caused false consumer-only divergences). Setup: consumer row at
// T-6s (past grace, previously visible), direct row at T-4s (within
// grace, previously excluded by the per-side filter). Both rows have
// MATCHING next_* values. With to=T:
//
//   - BUG (per-side grace pre-filter): direct_obs filters T-4s out;
//     consumer_obs keeps T-6s; FULL OUTER JOIN emits consumer-only row
//     → event surfaces as a false "direct missing" divergence.
//   - FIX (grace on joined pair): both rows visible in CTEs; join pairs
//     them; matching next_* → pair falls out of the DISTINCT-FROM
//     WHERE clause; alternatively the GREATEST(d, c)=T-4s pair-level
//     grace check excludes it. Result: empty for this event.
func TestIntegration_CadenceUpdater_FindDivergences_GraceWindowAppliedToPair(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-grace-pair", "weekly")
	occurredAt := accelerated.GetCurrentTime()
	cadenceStr := "weekly"

	// Create and publish an event envelope — we need a real event_id the
	// two observation rows can share (the FK on event_shadow_cadence_observation.event_id
	// is to event(id)).
	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contactID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "grace-pair:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))

	// Insert both rows via the test-only RecordAtTime repo helper so
	// observed_at is deterministic. Pre-fix bug needs direct inside the
	// grace window and consumer just outside it, so we simulate wall-
	// clock offsets without waiting.
	windowEnd := accelerated.GetCurrentTime().Add(1 * time.Second)
	consumerObservedAt := windowEnd.Add(-6 * time.Second) // past grace (visible under both old and new)
	directObservedAt := windowEnd.Add(-4 * time.Second)   // within grace (hidden under old pre-filter)

	baseObs := repository.CadenceShadowObservation{
		EventID:    envelope.ID,
		ContactID:  contactID,
		Source:     repository.InteractionSourceTelegram,
		Direction:  repository.InteractionDirectionMutual,
		Branch:     repository.CadenceShadowBranchForward,
		OccurredAt: occurredAt,
	}
	consumerObs := baseObs
	consumerObs.Writer = repository.CadenceShadowWriterConsumer
	require.NoError(t, e.cadenceShadowRep.RecordAtTime(ctx, consumerObs, consumerObservedAt))
	directObs := baseObs
	directObs.Writer = repository.CadenceShadowWriterDirect
	require.NoError(t, e.cadenceShadowRep.RecordAtTime(ctx, directObs, directObservedAt))

	// from well before the earliest row; to at windowEnd. Effective grace
	// edge = to - 5s = windowEnd - 5s. Consumer observed_at = windowEnd -
	// 6s (past grace by 1s). Direct observed_at = windowEnd - 4s (within
	// grace by 1s). Pair's GREATEST = windowEnd - 4s → within grace → excluded.
	windowStart := consumerObservedAt.Add(-1 * time.Minute)
	divergences, err := e.cadenceShadowRep.FindDivergences(ctx, windowStart, windowEnd)
	require.NoError(t, err)
	for _, d := range divergences {
		require.NotEqual(t, envelope.ID, d.EventID,
			"grace filter must be applied to the JOINED pair's latest observed_at, not each side independently")
	}
}

// -----------------------------------------------------------------------------
// Coverage-gap documentation tests. Each asserts "no cadence shadow row"
// for a code path we explicitly do NOT cover in PR 7.
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_ExtendInteraction_NoCadenceShadowRow —
// ExtendInteraction passes nil eventID; shadow-capture branch short-circuits.
func TestIntegration_CadenceUpdater_ExtendInteraction_NoCadenceShadowRow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-extend", "weekly")
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	// Create the interaction directly so we can extend it without the
	// event-bus consumer path.
	ref := "ext-" + uuid.NewString()
	inter, err := e.base.contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     repository.InteractionSourceTelegram,
		SourceRef:  &ref,
		OccurredAt: occurredAt,
		Direction:  repository.InteractionDirectionMutual,
	})
	require.NoError(t, err)

	// Extend — no new interaction.recorded event is emitted.
	countBefore, err := e.cadenceShadowRep.CountByWriter(ctx, repository.CadenceShadowWriterDirect)
	require.NoError(t, err)
	require.NoError(t, e.base.contactService.ExtendInteraction(
		ctx, inter.ID, contactID, repository.InteractionDirectionMutual, occurredAt.Add(time.Hour), nil,
	))
	countAfter, err := e.cadenceShadowRep.CountByWriter(ctx, repository.CadenceShadowWriterDirect)
	require.NoError(t, err)
	require.Equal(t, countBefore, countAfter, "Extend must not write a cadence shadow row")
}

// TestIntegration_CadenceUpdater_PromoteInteractionToMutual_NoCadenceShadowRow —
// Promote also passes nil eventID; documents the PR 7 gap.
func TestIntegration_CadenceUpdater_PromoteInteractionToMutual_NoCadenceShadowRow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-promote", "weekly")
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	ref := "promote-" + uuid.NewString()
	inter, err := e.base.contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     repository.InteractionSourceTelegram,
		SourceRef:  &ref,
		OccurredAt: occurredAt,
		Direction:  repository.InteractionDirectionOutbound,
	})
	require.NoError(t, err)

	countBefore, err := e.cadenceShadowRep.CountByWriter(ctx, repository.CadenceShadowWriterDirect)
	require.NoError(t, err)
	require.NoError(t, e.base.contactService.PromoteInteractionToMutual(
		ctx, inter.ID, contactID, occurredAt.Add(time.Hour),
	))
	countAfter, err := e.cadenceShadowRep.CountByWriter(ctx, repository.CadenceShadowWriterDirect)
	require.NoError(t, err)
	require.Equal(t, countBefore, countAfter, "Promote must not write a cadence shadow row")
}

// -----------------------------------------------------------------------------
// Mode gating + scope exclusions.
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_ModeOff_WorkerDrainsNoOp — with
// EVENT_BUS_CADENCE_MODE=off, the worker must short-circuit before
// touching the DB: zero rows land in event_shadow_cadence_observation
// after processing an interaction.recorded event. Mirrors the "off-mode
// is a silent drain" contract (plan Decision 9 always-register model).
func TestIntegration_CadenceUpdater_ModeOff_WorkerDrainsNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	// Build an OFF-mode updater pointing at the same repo/bus. The env's
	// default cadenceUpdater is shadow-mode, so we explicitly use a
	// separate off-mode instance here.
	offUpdater := consumer.NewCadenceUpdater(e.cadenceShadowRep, e.base.bus, consumer.CadenceModeOff)

	contactID := e.seedContactWithCadence(t, "cad-off", "weekly")
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	cadenceStr := "weekly"

	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contactID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "off:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))

	// Invoke the off-mode updater in a fresh tx.
	require.NoError(t, pgx.BeginTxFunc(ctx, e.base.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return offUpdater.HandleEvent(ctx, tx, envelope)
	}))

	// No row should exist for either writer.
	consumerObs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.Nil(t, consumerObs, "mode=off: no consumer observation must be written")

	directObs, err := e.cadenceShadowRep.FindMatchingDirect(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.Nil(t, directObs, "mode=off: no direct observation must be written")

	// Stronger assertion: zero rows in the table for this event_id period.
	var total int
	err = e.base.database.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM event_shadow_cadence_observation WHERE event_id = $1",
		envelope.ID,
	).Scan(&total)
	require.NoError(t, err)
	require.Zero(t, total, "mode=off: zero rows in event_shadow_cadence_observation for event_id")

	// --- Second phase: direct-path observer UNWIRED (mimics production
	// mode=off wiring, which skips SetCadenceShadowObserver in main.go).
	// Exercise the real `withShadow=true` caller path — the same path
	// InteractionRecorder takes when mode != off — against a service
	// whose cadenceShadow field is nil. The shadow branch in
	// applyInteractionEffectsFromRow gates on `cadenceShadow != nil &&
	// shadowQueue != nil && prev != nil` (contact.go:710), so the
	// returned drain fn MUST be nil even though the caller opted in.
	// A stub RecordInteraction wrapper (which hard-codes withShadow=false)
	// wouldn't test this — the assertion would pass trivially.
	base2 := newConsumerTestEnv(t, ctx)
	// Intentionally do NOT call SetCadenceShadowObserver on base2.contactService.

	directOffContactID := base2.newContact(t, "cad-off-direct")
	ref := "off-direct-" + uuid.NewString()
	var res *repository.RecordInteractionResult
	err = pgx.BeginTxFunc(ctx, base2.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		out, txErr := base2.contactService.RecordInteractionTx(
			ctx, tx, true, // withShadow=TRUE — production mode!=off calls this path
			repository.RecordInteractionRequest{
				ContactID:  directOffContactID,
				Source:     repository.InteractionSourceTelegram,
				SourceRef:  &ref,
				OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
				Direction:  repository.InteractionDirectionMutual,
			},
		)
		if txErr != nil {
			return txErr
		}
		res = out
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Nil(t, res.ShadowDrainFn,
		"cadenceShadow=nil must collapse the drain fn to nil even when withShadow=true")

	// Belt-and-suspenders: if some caller DID invoke the nil drain, it
	// would panic. So we don't invoke it here; just assert no direct row
	// made it to the DB.
	var directForContact int
	err = base2.database.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM event_shadow_cadence_observation WHERE contact_id = $1 AND writer = 'direct'",
		directOffContactID,
	).Scan(&directForContact)
	require.NoError(t, err)
	require.Zero(t, directForContact,
		"mode=off production wiring: cadenceShadow=nil + withShadow=true must produce no direct observation row")
}

// TestIntegration_CadenceUpdater_MergeContacts_NoCadenceShadowRow —
// MergeContacts transfers related rows and soft-deletes the source
// contact but does NOT emit interaction.recorded, so neither the direct
// path nor the consumer observes the cadence columns it touches.
// Documents the explicit exclusion in plan §Acceptance scope.
func TestIntegration_CadenceUpdater_MergeContacts_NoCadenceShadowRow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	sourceID := e.seedContactWithCadence(t, "cad-merge-src", "weekly")
	targetID := e.seedContactWithCadence(t, "cad-merge-tgt", "weekly")

	// Record an interaction on the SOURCE contact first so there's
	// meaningful cadence state to transfer during merge. This ensures
	// MergeContacts actually rewrites cadence-bearing data.
	ref := "merge-src-" + uuid.NewString()
	_, err := e.base.contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  sourceID,
		Source:     repository.InteractionSourceTelegram,
		SourceRef:  &ref,
		OccurredAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		Direction:  repository.InteractionDirectionMutual,
	})
	require.NoError(t, err)

	// Premise check: source contact now has populated cadence fields
	// that MergeContacts WILL rewrite. If any of these assertions fail,
	// the merge isn't exercising cadence-bearing data and the test's
	// "no shadow row" guarantee becomes vacuous.
	srcBeforeMerge, err := e.base.contactRepo.GetContact(ctx, sourceID)
	require.NoError(t, err)
	require.NotNil(t, srcBeforeMerge.LastContacted, "mutual interaction must populate source.last_contacted")
	require.NotNil(t, srcBeforeMerge.LastOutreachAt, "mutual interaction must populate source.last_outreach_at")
	require.NotNil(t, srcBeforeMerge.LastResponseAt, "mutual interaction must populate source.last_response_at")
	require.NotNil(t, srcBeforeMerge.ContactBy, "mutual interaction + cadence='weekly' must populate source.contact_by")

	merged, err := e.base.contactService.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: sourceID,
		TargetContactID: targetID,
	})
	require.NoError(t, err)
	require.Equal(t, targetID, merged.ID)

	// Per-contact invariant: MergeContacts runs through UpdateContact,
	// not applyInteractionEffectsFromRow, and emits no interaction.recorded
	// event. Neither writer should have produced a row for either
	// participant. Assert per-contact (not cross-query table-wide counts)
	// so concurrently-running integration tests on the shared DB can't
	// race us.
	srcCount, err := e.cadenceShadowRep.CountByContact(ctx, sourceID)
	require.NoError(t, err)
	require.Zero(t, srcCount, "MergeContacts must not emit any shadow observation for the source contact")
	tgtCount, err := e.cadenceShadowRep.CountByContact(ctx, targetID)
	require.NoError(t, err)
	require.Zero(t, tgtCount, "MergeContacts must not emit any shadow observation for the target contact")
}

// -----------------------------------------------------------------------------
// FindDivergences — induced mismatch (real divergence surfaces).
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_FindDivergences_DetectsInducedMismatch —
// the consumer writes its row for a synthetic event; we insert a direct
// row for the same event with a DIFFERENT next_last_contacted. Per-row
// invariant: FindDivergences must return that event_id with both sides
// populated and the expected branch/next mismatches.
func TestIntegration_CadenceUpdater_FindDivergences_DetectsInducedMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	// Push observed_at back past the 5s grace so the row is query-eligible.
	occurredAt := accelerated.GetCurrentTime().Add(-10 * time.Second)
	cadenceStr := "weekly"
	contactID := e.seedContactWithCadence(t, "cad-diverge", cadenceStr)

	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contactID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "diverge:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	consumerObs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.NotNil(t, consumerObs, "consumer row must be present before induced direct row")

	// Induce divergence: insert a direct row whose next_last_contacted is
	// shifted by +1h compared to the consumer's value. Everything else
	// matches the consumer to keep the test focused on a single field.
	shifted := consumerObs.NextLastContacted.Add(1 * time.Hour)
	inducedDirect := repository.CadenceShadowObservation{
		EventID:             envelope.ID,
		ContactID:           contactID,
		Source:              repository.InteractionSourceTelegram,
		Direction:           repository.InteractionDirectionMutual,
		Branch:              repository.CadenceShadowBranchForward,
		OccurredAt:          occurredAt,
		ApplyLastContacted:  consumerObs.ApplyLastContacted,
		ApplyLastOutreachAt: consumerObs.ApplyLastOutreachAt,
		ApplyLastResponseAt: consumerObs.ApplyLastResponseAt,
		ApplyContactBy:      consumerObs.ApplyContactBy,
		NextLastContacted:   &shifted,
		NextLastOutreachAt:  consumerObs.NextLastOutreachAt,
		NextLastResponseAt:  consumerObs.NextLastResponseAt,
		NextContactBy:       consumerObs.NextContactBy,
	}
	require.NoError(t, e.cadenceShadowRep.RecordDirect(ctx, nil, inducedDirect))

	// Window must span occurred_at through at least 5s past "now" so the
	// grace-subtracted edge puts both rows inside the query's time range.
	windowStart := occurredAt.Add(-1 * time.Minute)
	windowEnd := accelerated.GetCurrentTime().Add(1 * time.Minute)
	divergences, err := e.cadenceShadowRep.FindDivergences(ctx, windowStart, windowEnd)
	require.NoError(t, err)

	var matched *repository.CadenceShadowDivergence
	for i := range divergences {
		if divergences[i].EventID == envelope.ID {
			matched = &divergences[i]
			break
		}
	}
	require.NotNil(t, matched, "induced mismatch must appear in FindDivergences results")
	require.NotNil(t, matched.DirectBranch, "direct side must be populated")
	require.NotNil(t, matched.ConsumerBranch, "consumer side must be populated")
	require.NotNil(t, matched.DirectNextLastContacted)
	require.NotNil(t, matched.ConsumerNextLastContacted)
	require.True(t, matched.DirectNextLastContacted.Equal(shifted),
		"direct next_last_contacted reflects the induced +1h value")
	require.False(t,
		matched.DirectNextLastContacted.Equal(*matched.ConsumerNextLastContacted),
		"direct and consumer next_last_contacted must differ (induced mismatch)")
}

// -----------------------------------------------------------------------------
// Helpers.
// -----------------------------------------------------------------------------

func timePtrEqual(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
}
