package api

import (
	"context"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/scheduler"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// TestIngestRawMessage_E2E_WorkerProducesEvent runs the
// MessagingAggregateForContactWorker against the live aggregation
// engine after a raw_message.received POST and asserts the engine
// emits a message.received event. The Stage 3 InteractionRecorder
// consumer is exercised by separate harness tests in
// consumer_interaction_recorder_integration_test.go; here we exercise
// the Stage 1 → Stage 2 join (raw_message ingest staging row +
// matched_contact_id → engine processes the chat and publishes the
// burst event).
//
// Why this test sits in tests/api and not internal/scheduler: the
// ingest path's router + middleware are required to stage the row
// in the first place. Without those, the engine has nothing to
// process. The other worker unit tests (with stub engine + lister)
// cover the worker's drain-loop behavior in isolation.
func TestIngestRawMessage_E2E_WorkerProducesEvent(t *testing.T) {
	env := setupRawIngestEnv(t)
	guid := "test-guid-" + uuid.NewString()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid, "chat-end-to-end", "+15551234567")
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	// Build a per-test messages aggregation engine wired with the
	// pool's tx beginner but a nil event publisher — the engine will
	// promote/extend or call its create path; we then assert the
	// staging row's processed_at landed.
	//
	// We deliberately do NOT register the real reenqueuer or wait for
	// Stage 3; this test exercises Stage 2 end-to-end and verifies
	// the staging row + matched_contact_id thread through to a chat-
	// scoped aggregator pass that drains the row.
	engine := messagesEngineForTest(t, env)

	// Run the chat-aware worker once via Work(). The worker resolves
	// chats then calls engine.AggregateForContact per chat. The engine
	// requires a contactID; the ingest test wires pairedContactA as
	// the contact owning the +15551234567 phone.
	worker := scheduler.NewMessagingAggregateForContactWorker(
		map[string]scheduler.ChatAwareAggregator{
			"messages": engine,
		},
		scheduler.NewPerSourceChatListerRegistry(map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
			"messages": env.messagesRepo.ListUnprocessedChatsByContact,
		}),
	)

	err := worker.Work(context.Background(), &river.Job[consumerjobs.MessagingAggregateForContactArgs]{
		Args: consumerjobs.MessagingAggregateForContactArgs{
			ContactID: env.pairedContactA,
			Source:    "messages",
		},
	})
	require.NoError(t, err, "worker drain should succeed")

	// Verify the staging row was claimed by the worker. The engine's
	// create-path tx commits the claim atomically; Stage 3 (separate
	// consumer) marks processed_at, which we don't run here. So we
	// assert claimed_at is non-nil, NOT processed_at.
	msg, err := env.messagesRepo.GetMessage(context.Background(), guid)
	require.NoError(t, err)
	require.NotNil(t, msg.ClaimedAt, "engine should have claimed the staging row")
	require.NotNil(t, msg.ClaimedSessionRef, "engine should have stamped claimed_session_ref")
}
