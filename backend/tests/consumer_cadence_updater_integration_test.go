package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

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

	cu := consumer.NewCadenceUpdater(base.contactRepo, shadowRepo, base.bus, consumer.CadenceModeShadow)

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
// V1 payload rejection (HTTP-ingested external producer without V2 upgrade).
// -----------------------------------------------------------------------------

// TestIntegration_CadenceUpdater_HTTPIngested_V1_Rejected — simulates a
// V1-shape interaction.recorded envelope hitting the consumer. Expected:
// ERROR log + nil return (no observation row). Documents the known
// limitation that PR 7 will be tightened by PR 8 at the HTTP boundary.
func TestIntegration_CadenceUpdater_HTTPIngested_V1_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-v1-reject", "weekly")
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	// V1 payload — no PrevCadenceSnapshot / PrevCadenceValue.
	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:       1,
		ContactID:     contactID,
		InteractionID: uuid.New(),
		Direction:     repository.InteractionDirectionMutual,
		OccurredAt:    occurredAt,
		Source:        repository.InteractionSourceTelegram,
	})
	require.NoError(t, err)
	envelope := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "v1-rej:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))

	// Consumer should log ERROR and succeed the job — no observation.
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	obs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.Nil(t, obs, "V1 payload: no consumer observation written")
}

// TestIntegration_CadenceUpdater_HTTPIngested_V2_ConsumerOnly — simulates
// an external producer that emits V2 with PrevCadenceSnapshot. Expected:
// consumer row written; no direct row (no in-process direct-path writer).
func TestIntegration_CadenceUpdater_HTTPIngested_V2_ConsumerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newCadenceIntegrationEnv(t, ctx)

	contactID := e.seedContactWithCadence(t, "cad-v2-ingest", "weekly")
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
		SourceID:   "v2-ingest:" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	require.NoError(t, e.base.bus.Publish(ctx, envelope))
	require.NoError(t, e.runCadenceHandleEvent(t, envelope))

	obs, err := e.cadenceShadowRep.FindMatchingConsumer(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.NotNil(t, obs, "V2 ingested event: consumer row written")

	direct, err := e.cadenceShadowRep.FindMatchingDirect(ctx, nil, envelope.ID)
	require.NoError(t, err)
	require.Nil(t, direct, "V2 ingested event: no direct row (no in-process direct-path writer)")
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
