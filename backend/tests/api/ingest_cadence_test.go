package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// End-to-end tests for PR 7 CadenceUpdater driven by the real HTTP ingest
// handler (api/handlers/ingest.go). Replaces the earlier bus.Publish-based
// stubs in tests/consumer_cadence_updater_integration_test.go. Exercises:
//
//   1. V1 interaction.recorded → ingested successfully (envelope shape is
//      valid) but CadenceUpdater rejects the payload (Version<2) with no
//      observation written.
//   2. V2 interaction.recorded → ingested successfully; CadenceUpdater
//      writes a consumer observation. No direct observation exists
//      because externally-ingested events have no in-process direct-path
//      writer (plan Decision 4).
//
// The ingest test router runs river with TestOnly=true so the cadence
// worker is registered but never picks up jobs — the tests invoke
// CadenceUpdater.HandleEvent synchronously after the HTTP POST to
// simulate the river worker step.
// -----------------------------------------------------------------------------

// postCadenceIngestEvent posts a single interaction.recorded event to the
// ingest endpoint and returns the server-assigned event envelope after a
// DB round-trip. Fails the test on any non-200 / 0-accepted path.
func postCadenceIngestEvent(
	t *testing.T,
	setup *ingestTestSetup,
	source, sourceID string,
	payload json.RawMessage,
	observedAt time.Time,
) *events.Envelope {
	t.Helper()
	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{{
			Source:     source,
			SourceID:   sourceID,
			Kind:       string(events.KindInteractionRecorded),
			Payload:    payload,
			ObservedAt: &observedAt,
		}},
	}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Accepted, "ingest batch should have accepted 1 event; got %+v", resp)

	// Look up the DB-assigned event row so callers have the real ID for
	// downstream shadow-observation assertions.
	env, err := setup.eventRepo.FindEventBySource(setup.ctx, source, sourceID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, env.ID, "event must have a populated ID after insert")
	return env
}

// TestIngest_CadenceUpdater_V1_Rejected — real HTTP ingest of a V1-shape
// interaction.recorded envelope. The handler accepts the envelope (V1 is
// a valid payload shape) but CadenceUpdater rejects it for version<2 and
// writes NO observation row. Documents the known PR 7 limitation that
// PR 8 will tighten at the HTTP boundary.
func TestIngest_CadenceUpdater_V1_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	// Seed a contact so V2 semantics would be able to compute next_* — we
	// want to prove V1 is rejected for payload-shape reasons only.
	contactRepo := repository.NewContactRepository(setup.database.Queries)
	cadencePtr := "weekly"
	contact, err := contactRepo.CreateContact(setup.ctx, repository.CreateContactRequest{
		FullName: "ingest-cadence-v1-" + uuid.NewString()[:8],
		Cadence:  &cadencePtr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(setup.ctx, contact.ID) })

	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:       1,
		ContactID:     contact.ID,
		InteractionID: uuid.New(),
		Direction:     repository.InteractionDirectionMutual,
		OccurredAt:    occurredAt,
		Source:        repository.InteractionSourceTelegram,
	})
	require.NoError(t, err)

	source := uniqueIngestSource("cadence-v1")
	env := postCadenceIngestEvent(t, setup, source, uuid.NewString(), payload, occurredAt)

	// Run the real CadenceUpdater against the ingested event. Shadow mode
	// is the production default under PR 7.
	cadenceShadowRepo := repository.NewCadenceShadowObservationRepository(setup.database.Queries, setup.database.Pool)
	updater := consumer.NewCadenceUpdater(
		cadenceShadowRepo,
		setup.busFactory(setup.eventRepo),
		consumer.CadenceModeShadow,
	)
	require.NoError(t, pgx.BeginTxFunc(setup.ctx, setup.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return updater.HandleEvent(setup.ctx, tx, env)
	}))

	consumerObs, err := cadenceShadowRepo.FindMatchingConsumer(setup.ctx, nil, env.ID)
	require.NoError(t, err)
	require.Nil(t, consumerObs, "V1 payload: no consumer observation written")

	directObs, err := cadenceShadowRepo.FindMatchingDirect(setup.ctx, nil, env.ID)
	require.NoError(t, err)
	require.Nil(t, directObs, "V1 payload: no direct observation (ingested events have no in-process direct writer)")
}

// TestIngest_CadenceUpdater_V3_Rejected — real HTTP ingest of an envelope
// whose payload advertises Version=3. The landed consumer uses
// `p.Version != 2` as the guard (reviewer's Risky-2 fix) so V3+ payloads
// are treated the same as V1: rejected with an ERROR log, no observation
// written. Belt-and-suspenders against future publishers that bump the
// version without updating this consumer first.
func TestIngest_CadenceUpdater_V3_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	contactRepo := repository.NewContactRepository(setup.database.Queries)
	cadencePtr := "weekly"
	contact, err := contactRepo.CreateContact(setup.ctx, repository.CreateContactRequest{
		FullName: "ingest-cadence-v3-" + uuid.NewString()[:8],
		Cadence:  &cadencePtr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(setup.ctx, contact.ID) })

	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	cadenceStr := "weekly"
	// Construct a V3 payload: bump Version to 3 but otherwise look like V2
	// (so ValidatePayload passes structurally). The consumer's Version
	// guard should still reject.
	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             3,
		ContactID:           contact.ID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)

	source := uniqueIngestSource("cadence-v3")
	env := postCadenceIngestEvent(t, setup, source, uuid.NewString(), payload, occurredAt)

	cadenceShadowRepo := repository.NewCadenceShadowObservationRepository(setup.database.Queries, setup.database.Pool)
	updater := consumer.NewCadenceUpdater(
		cadenceShadowRepo,
		setup.busFactory(setup.eventRepo),
		consumer.CadenceModeShadow,
	)
	require.NoError(t, pgx.BeginTxFunc(setup.ctx, setup.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return updater.HandleEvent(setup.ctx, tx, env)
	}))

	consumerObs, err := cadenceShadowRepo.FindMatchingConsumer(setup.ctx, nil, env.ID)
	require.NoError(t, err)
	require.Nil(t, consumerObs, "V3 payload: Version != 2 guard must reject without writing an observation")

	directObs, err := cadenceShadowRepo.FindMatchingDirect(setup.ctx, nil, env.ID)
	require.NoError(t, err)
	require.Nil(t, directObs, "V3 payload: no direct observation (ingested events have no in-process direct writer)")
}

// TestIngest_CadenceUpdater_V2_ConsumerOnly — real HTTP ingest of a V2
// interaction.recorded envelope carrying PrevCadenceSnapshot +
// PrevCadenceValue. CadenceUpdater writes a consumer observation; no
// direct row exists because the ingest path has no in-process direct-
// path writer. The consumer row's next_* values reflect the V2 payload.
func TestIngest_CadenceUpdater_V2_ConsumerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	contactRepo := repository.NewContactRepository(setup.database.Queries)
	cadencePtr := "weekly"
	contact, err := contactRepo.CreateContact(setup.ctx, repository.CreateContactRequest{
		FullName: "ingest-cadence-v2-" + uuid.NewString()[:8],
		Cadence:  &cadencePtr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(setup.ctx, contact.ID) })

	// Push observed_at back past the 5s grace window so the row is
	// query-eligible for FindDivergences, should callers extend this test.
	occurredAt := accelerated.GetCurrentTime().Add(-10 * time.Second)
	cadenceStr := "weekly"
	payload, err := events.Marshal(events.KindInteractionRecorded, events.InteractionRecordedPayload{
		Version:             2,
		ContactID:           contact.ID,
		InteractionID:       uuid.New(),
		Direction:           repository.InteractionDirectionMutual,
		OccurredAt:          occurredAt,
		Source:              repository.InteractionSourceTelegram,
		PrevCadenceSnapshot: &events.CadenceFieldsSnapshot{},
		PrevCadenceValue:    &cadenceStr,
	})
	require.NoError(t, err)

	source := uniqueIngestSource("cadence-v2")
	env := postCadenceIngestEvent(t, setup, source, uuid.NewString(), payload, occurredAt)

	cadenceShadowRepo := repository.NewCadenceShadowObservationRepository(setup.database.Queries, setup.database.Pool)
	updater := consumer.NewCadenceUpdater(
		cadenceShadowRepo,
		setup.busFactory(setup.eventRepo),
		consumer.CadenceModeShadow,
	)
	require.NoError(t, pgx.BeginTxFunc(setup.ctx, setup.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return updater.HandleEvent(setup.ctx, tx, env)
	}))

	consumerObs, err := cadenceShadowRepo.FindMatchingConsumer(setup.ctx, nil, env.ID)
	require.NoError(t, err)
	require.NotNil(t, consumerObs, "V2 ingested event: consumer row must be written")
	require.Equal(t, repository.CadenceShadowBranchForward, consumerObs.Branch)
	// Mutual direction sets all four apply flags true.
	require.True(t, consumerObs.ApplyLastContacted)
	require.True(t, consumerObs.ApplyLastOutreachAt)
	require.True(t, consumerObs.ApplyLastResponseAt)
	require.True(t, consumerObs.ApplyContactBy)

	directObs, err := cadenceShadowRepo.FindMatchingDirect(setup.ctx, nil, env.ID)
	require.NoError(t, err)
	require.Nil(t, directObs, "V2 ingested event: no direct row (no in-process direct-path writer)")
}
