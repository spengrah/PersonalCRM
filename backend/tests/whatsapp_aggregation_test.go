package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// whatsappTestEnv bundles everything a WhatsApp engine integration test needs.
type whatsappTestEnv struct {
	ctx             context.Context
	database        *db.Database
	commsRepo       *repository.CommsMessageRepository
	interactionRepo *repository.InteractionRepository
	contactRepo     *repository.ContactRepository
	engine          *aggregation.Engine
}

// setupWhatsAppEngineTest wires the FULL create-path harness, mirroring
// setupGChatEngineTest: a live river client running the InteractionRecorder
// worker, a StagingProcessorRegistry carrying the whatsapp session-scoped
// processor, and a WhatsApp aggregation engine built with database.Pool as
// TxBeginner so the create path takes the ClaimRowsTx + PublishTx branch.
//
// It is the regression guard for TWO seams a missing entry would silence: the
// whatsapp StagingProcessor (without it the recorder's zero-rows-affected
// rollback fires and the engine reprocesses forever) and
// consumer.messageInteractionSources (without the whatsapp entry the recorder
// rejects the source outright and no interaction is ever written).
func setupWhatsAppEngineTest(t *testing.T) *whatsappTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	bus, contactService := setupWhatsAppEventBus(t, ctx, database, commsRepo)

	engine := wapkg.NewAggregationEngine(
		2, 48, // the WhatsApp defaults (DefaultWhatsAppBurstWindowHours / ReplyBridgeHours)
		commsRepo, interactionRepo,
		contactService, contactService,
		bus,
		database.Pool, // TxBeginner: create path takes ClaimRowsTx + PublishTx
		consumer.NewRiverInteractionRecorderEnqueuer(nil),
	)

	return &whatsappTestEnv{
		ctx:             ctx,
		database:        database,
		commsRepo:       commsRepo,
		interactionRepo: interactionRepo,
		contactRepo:     contactRepo,
		engine:          engine,
	}
}

// setupWhatsAppEventBus mirrors setupGChatEventBus but registers the WHATSAPP
// session-scoped StagingProcessor.
func setupWhatsAppEventBus(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	commsRepo *repository.CommsMessageRepository,
) (*events.Bus, *service.ContactService) {
	t.Helper()

	eventRepo := repository.NewEventRepository(database.Queries)

	workers := river.NewWorkers()
	shim := &deferredRecorderWorker{}
	river.AddWorker(workers, shim)
	river.AddWorker(workers, &cadenceUpdaterNoopWorker{})
	river.AddWorker(workers, &followUpManagerNoopWorker{})
	river.AddWorker(workers, &emailInteractionNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 2},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	assertSvc, cache := buildKnowledgeDeps(t, database, bus)
	contactService := service.NewContactService(
		database, contactRepo,
		repository.NewContactMethodRepository(database.Queries),
		repository.NewInteractionRepository(database.Queries),
		repository.NewContactTaskRepository(database.Queries),
		nil, nil,
		cadenceUpdater, assertSvc, cache, nil,
	)
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceWhatsApp: repository.NewCommsSessionStagingProcessor(commsRepo),
	})
	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry, bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	shim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus, contactService
}

// newWhatsAppContact creates a contact and registers cleanup for its rows.
func (e *whatsappTestEnv) newWhatsAppContact(t *testing.T, name string) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

// seedWhatsAppRow stages one comms_message(source='whatsapp') row. contactID
// may be uuid.Nil, which stages the row UNMATCHED (legal for whatsapp, unlike
// gmail/gchat).
func (e *whatsappTestEnv) seedWhatsAppRow(t *testing.T, contactID *uuid.UUID, chatJID, externalID, direction string, sentAt time.Time) *repository.CommsMessage {
	t.Helper()
	body := "whatsapp body"
	peer := chatJID
	row, err := e.commsRepo.UpsertChatMessage(e.ctx, repository.UpsertChatMessageParams{
		Source:           repository.InteractionSourceWhatsApp,
		ExternalID:       externalID,
		ThreadID:         chatJID,
		Body:             &body,
		PeerHandle:       &peer,
		Direction:        direction,
		SentAt:           sentAt,
		MatchedContactID: contactID,
	})
	require.NoError(t, err)
	return row
}

// waitForWhatsAppRowsProcessed polls each row until all carry processed_at +
// the expected interaction_id. Routes through the repository (no raw SQL).
func waitForWhatsAppRowsProcessed(t *testing.T, e *whatsappTestEnv, ids []uuid.UUID, interactionID uuid.UUID) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		all := true
		for _, id := range ids {
			row, err := e.commsRepo.GetByID(e.ctx, id)
			require.NoError(t, err)
			if row.ProcessedAt == nil || row.InteractionID == nil || *row.InteractionID != interactionID {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d whatsapp rows to be processed + linked to interaction %s", len(ids), interactionID)
}

// TestWhatsAppPipeline_StagedRowBecomesInteraction is the pipeline half of the
// aggregation proof: two staged inbound rows in one chat become ONE interaction
// with the WhatsApp wire format, both rows are marked processed, and the
// contact's last_contacted moves.
//
// spec: WHA-040.interaction-per-burst
// spec: WHA-040.source-is-whatsapp
// spec: WHA-040.source-ref-is-chat-scoped
// spec: WHA-040.description-names-whatsapp
// spec: WHA-040.rows-marked-processed
// spec: WHA-040.last-contacted-moves
func TestWhatsAppPipeline_StagedRowBecomesInteraction(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Pipeline "+suffix)
	require.Nil(t, contact.LastContacted, "a freshly created contact has never been contacted")

	chatJID := "1204555" + suffix + "@s.whatsapp.net"
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	r1 := e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-pipe1-"+suffix, repository.InteractionDirectionInbound, base)
	r2 := e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-pipe2-"+suffix, repository.InteractionDirectionInbound, base.Add(20*time.Minute))

	require.NoError(t, e.engine.AggregateAll(e.ctx))

	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)
	assert.Equal(t, repository.InteractionSourceWhatsApp, interactions[0].Source)
	assert.Equal(t, repository.InteractionDirectionInbound, interactions[0].Direction)
	require.NotNil(t, interactions[0].SourceRef)
	assert.Equal(t, "whatsapp:"+chatJID+":wa-pipe1-"+suffix, *interactions[0].SourceRef)
	require.NotNil(t, interactions[0].Description)
	assert.Equal(t, "WhatsApp response (2 messages)", *interactions[0].Description)

	waitForWhatsAppRowsProcessed(t, e, []uuid.UUID{r1.ID, r2.ID}, interactions[0].ID)

	moved, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, moved.LastContacted, "an inbound WhatsApp interaction must move last_contacted")
}

// TestWhatsAppAggregation_BurstExtends: a third same-direction message inside
// the 2h burst window extends the existing interaction instead of creating a
// second one.
//
// spec: WHA-040.interaction-per-burst
func TestWhatsAppAggregation_BurstExtends(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Burst "+suffix)
	chatJID := "1204555" + suffix + "@s.whatsapp.net"
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-burst1-"+suffix, repository.InteractionDirectionOutbound, base)
	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-burst2-"+suffix, repository.InteractionDirectionOutbound, base.Add(20*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))
	first := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	require.NotNil(t, first[0].Description)
	require.Equal(t, "WhatsApp outreach (2 messages)", *first[0].Description)

	// A third outbound inside the burst window extends the SAME interaction:
	// the count stays at one and the new row links to the interaction that
	// already existed. (The extender rewrites the description from the NEW
	// session, so the text is not the evidence — the linkage is.)
	r3 := e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-burst3-"+suffix, repository.InteractionDirectionOutbound, base.Add(40*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))

	waitForWhatsAppRowsProcessed(t, e, []uuid.UUID{r3.ID}, first[0].ID)
	rows, err := e.interactionRepo.ListContactInteractions(e.ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the burst must extend the existing interaction, not create a second one")
	assert.Equal(t, first[0].ID, rows[0].ID)
}

// TestWhatsAppAggregation_ReplyBridgePromotesToMutual: an inbound message
// inside the 48h reply bridge promotes the outbound interaction to mutual
// rather than creating a second one.
func TestWhatsAppAggregation_ReplyBridgePromotesToMutual(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Bridge "+suffix)
	chatJID := "1204555" + suffix + "@s.whatsapp.net"
	base := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)

	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-br-out1-"+suffix, repository.InteractionDirectionOutbound, base)
	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-br-out2-"+suffix, repository.InteractionDirectionOutbound, base.Add(5*time.Minute))
	// Inbound 55 minutes after the last outbound. The DIRECTION CHANGE is what
	// starts a new burst (groupIntoBursts splits on direction regardless of
	// gap); the gap being inside the 48h reply-bridge window is what merges the
	// two back into one mutual interaction.
	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-br-in1-"+suffix, repository.InteractionDirectionInbound, base.Add(time.Hour))

	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))

	rows := waitForInteractionDirection(t, e.ctx, e.interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
	require.Len(t, rows, 1, "the reply bridge must promote the outbound interaction, not add a second one")
	assert.Equal(t, repository.InteractionDirectionMutual, rows[0].Direction)
}

// TestWhatsAppAggregation_SameDirectionCoalesces: two same-direction bursts in
// the same chat, the second inside the first's burst window, coalesce into one
// interaction rather than two.
func TestWhatsAppAggregation_SameDirectionCoalesces(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Coalesce "+suffix)
	chatJID := "1204555" + suffix + "@s.whatsapp.net"
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-co1-"+suffix, repository.InteractionDirectionInbound, base)
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))
	first := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)

	// A second INBOUND message inside the burst window. The engine checks the
	// reply bridge first (no outbound to promote), then coalesces onto the
	// existing same-direction interaction rather than creating a second one.
	r2 := e.seedWhatsAppRow(t, &contact.ID, chatJID, "wa-co2-"+suffix, repository.InteractionDirectionInbound, base.Add(30*time.Minute))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))

	waitForWhatsAppRowsProcessed(t, e, []uuid.UUID{r2.ID}, first[0].ID)
	rows, err := e.interactionRepo.ListContactInteractions(e.ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a same-direction follow-up inside the burst window must coalesce")
	assert.Equal(t, first[0].ID, rows[0].ID)
}

// TestWhatsAppAggregation_UnmatchedRowIsNeverAggregated is I2's regression
// guard. A staged row with matched_contact_id IS NULL is legal for WhatsApp (a
// peer whose phone was never recovered), and it must never reach the claim path
// or produce an interaction.
//
// spec: WHA-041.unmatched-row-never-aggregated
// spec: WHA-041.attach-makes-it-eligible
func TestWhatsAppAggregation_UnmatchedRowIsNeverAggregated(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Unmatched "+suffix)
	chatJID := "1204555" + suffix + "@s.whatsapp.net"
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	// Staged WITHOUT a contact.
	row := e.seedWhatsAppRow(t, nil, chatJID, "wa-unmatched-"+suffix, repository.InteractionDirectionInbound, base)
	require.Nil(t, row.MatchedContactID)

	require.NoError(t, e.engine.AggregateAll(e.ctx))

	rows, err := e.interactionRepo.ListContactInteractions(e.ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	assert.Empty(t, rows, "an unattached whatsapp row must never become an interaction")

	after, err := e.commsRepo.GetByID(e.ctx, row.ID)
	require.NoError(t, err)
	assert.Nil(t, after.ProcessedAt, "an unattached row must stay unprocessed")
	assert.Nil(t, after.InteractionID)

	// Attaching it makes it eligible: the same engine pass now derives one.
	e.commsRepo.SetPool(e.database.Pool)
	peer := chatJID
	attached, _, err := e.commsRepo.AttachUnmatchedByPeer(e.ctx, repository.InteractionSourceWhatsApp, nil, &peer, contact.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, attached)

	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, chatJID))
	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, repository.InteractionSourceWhatsApp, interactions[0].Source)
}
