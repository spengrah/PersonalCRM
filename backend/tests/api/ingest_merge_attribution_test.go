package api

// End-to-end confirmation of the merge identity-attribution fix through the
// REAL ingest path: a raw_message from the loser's handle is ingested (seeding
// the identity cache), the loser is merged into the winner via the production
// MergeContacts, and a SECOND raw_message from the same handle must stage with
// matched_contact_id = the winner — not the tombstoned loser.

import (
	"context"
	"net/http"
	"testing"

	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newMergeCapableContactService wires a ContactService against the ingest
// env's per-test clone with the dependencies MergeContacts requires (cadence
// updater + knowledge writer + task-close enqueuer). The knowledge bus reuses
// the env's insert-only river client (apiKnowledgeCacheNoopWorker registers
// the assertion job kind).
func newMergeCapableContactService(t *testing.T, env *ingestRawTestEnv) *service.ContactService {
	t.Helper()
	database := env.database

	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)

	bus := events.NewBus(database.Pool, env.riverClient, eventRepo)
	assertSvc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)
	cache := consumer.NewKnowledgeCacheUpdater(assertionRepo, nodeRepo, env.contactRepo)

	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, env.contactRepo, database.Queries, consumer.CadenceModeCutover, false)

	svc := service.NewContactService(database, env.contactRepo, env.cmRepo, interactionRepo, taskRepo, nil, nil,
		cadenceUpdater, assertSvc, cache, nil)

	// No contact tasks are seeded in this env; wired defensively so a merge
	// that ever closes an eligible task takes the WARN-skip path, not the
	// wiring-bug error.
	svc.SetTaskCloseEnqueuer(env.riverClient, false)
	return svc
}

// TestIngestRawMessage_AfterMerge_AttributesToSurvivor is the faithful
// reproduction of the report: ingest a message from a loser handle, merge the
// loser into the winner, ingest again, and assert BOTH the historical and the
// new staging rows attribute to the winner.
func TestIngestRawMessage_AfterMerge_AttributesToSurvivor(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	ctx := context.Background()

	contactSvc := newMergeCapableContactService(t, env)

	// Winner + loser created through the production service path (so their
	// person nodes exist for the merge's graph step). The loser owns the
	// handle the daemon reports.
	const loserHandle = "+15559990001"
	winner, _, err := contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Merge E2E Winner",
	}, nil)
	require.NoError(t, err)
	loser, _, err := contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Merge E2E Loser",
	}, nil)
	require.NoError(t, err)
	_, err = env.cmRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: loser.ID,
		Type:      "phone",
		Value:     loserHandle,
	})
	require.NoError(t, err)

	// First ingest: discovery matches the loser and seeds the identity cache.
	guid1 := "merge-e2e-guid-" + uuid.NewString()
	ev1 := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid1, "merge-e2e-chat", loserHandle)
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev1},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors: %+v", resp.Errors)

	msg1, err := env.messagesRepo.GetMessage(ctx, guid1)
	require.NoError(t, err)
	require.NotNil(t, msg1.MatchedContactID)
	require.Equal(t, loser.ID, *msg1.MatchedContactID, "pre-merge attribution goes to the loser")

	// Merge the loser into the winner via the production path.
	_, err = contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: loser.ID,
		TargetContactID: winner.ID,
	})
	require.NoError(t, err)

	// Second ingest from the SAME handle: the cached identity must not
	// short-circuit to the tombstoned loser.
	guid2 := "merge-e2e-guid-" + uuid.NewString()
	ev2 := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid2, "merge-e2e-chat", loserHandle)
	w = postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev2},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp = parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors: %+v", resp.Errors)

	msg2, err := env.messagesRepo.GetMessage(ctx, guid2)
	require.NoError(t, err)
	require.NotNil(t, msg2.MatchedContactID)
	require.Equal(t, winner.ID, *msg2.MatchedContactID, "post-merge ingest attributes to the survivor")

	// The historical staging row was repointed by the merge as well.
	msg1After, err := env.messagesRepo.GetMessage(ctx, guid1)
	require.NoError(t, err)
	require.NotNil(t, msg1After.MatchedContactID)
	require.Equal(t, winner.ID, *msg1After.MatchedContactID, "pre-merge staging row repointed to the survivor")
}
