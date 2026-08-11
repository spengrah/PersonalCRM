//go:build integration_testdb

// Composition-root parity pin for the WhatsApp aggregation wiring.
//
// Every source-keyed fan-out site in the messaging pipeline is a map that is
// SILENTLY LENIENT on a miss: an unregistered source yields (0, nil) or
// (nil, nil) rather than an error, so a missing whatsapp entry is never a
// compile error and never a runtime error — it is a silently dead source. The
// golden-list test cannot see it (whatsapp registers no worker, periodic or
// provider), and backend/tests/ cannot reach these wire functions (package
// main). This test is the discriminating proof.
//
// EVERY assertion is an INVOCATION, never a presence check. A presence check
// (`wiring.Engines["whatsapp"] != nil`) is behaviorally a keyset check: it
// passes on the realistic copy-paste defect where the right KEY holds the wrong
// VALUE — a gchat engine under the whatsapp key, or a reenqueuer constructed
// with InteractionSourceGChat. Each assertion below therefore reaches its site
// through a call whose observable effect differs under a mis-binding.
package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/testdb"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// whatsappWiringFixture is one run's rows. Chats C, D and E are DISJOINT so no
// assertion consumes another's state: assertion 1 claims chat C, assertion 4
// aggregates chat D, and assertions 2+3 use chat E, which nothing else claims
// (MarkProcessedTx's underlying predicate is
// `claimed_session_ref = @session_ref OR claimed_session_ref IS NULL`, so a row
// the engine already claimed under its own session ref would return 0 for a
// reason having nothing to do with the registry).
type whatsappWiringFixture struct {
	contactID uuid.UUID
	chatC     string
	chatD     string
	chatE     string
	firstC    string // external_id of the first (oldest) row in chat C
	firstD    string
	rowE      uuid.UUID
	chatG     string // group JID, for the venue-kind discriminator
	rowG      uuid.UUID
	idPrefix  string
	refPrefix string
}

// seedWhatsAppWiringFixture writes one contact and four unprocessed
// comms_message(source='whatsapp') rows through the production upsert.
//
// Every external_id is per-run unique and a t.Cleanup hard-deletes the run's
// rows, interactions AND events by prefix. That is load-bearing for the
// ephemeral falsification protocol rather than hygiene: events.Bus.PublishTx
// dedups on (source, source_id), so a leftover event row with
// source_id = "whatsapp:<chatC>:<id>" from an earlier run would satisfy
// assertion 1 no matter what the engine did — and the protocol runs this test
// eleven times against the same database.
func seedWhatsAppWiringFixture(t *testing.T, ctx context.Context, chain wireChain) whatsappWiringFixture {
	t.Helper()

	run := uuid.NewString()
	fx := whatsappWiringFixture{
		chatC:     "1204555" + run[:4] + "1@s.whatsapp.net",
		chatD:     "1204555" + run[:4] + "2@s.whatsapp.net",
		chatE:     "1204555" + run[:4] + "3@s.whatsapp.net",
		chatG:     "1204555" + run[:4] + "4-1690000000@g.us",
		idPrefix:  "wa-wiring-" + run + "-",
		refPrefix: repository.InteractionSourceWhatsApp + ":1204555" + run[:4],
	}

	contact, err := chain.core.Contact.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "WhatsApp Wiring Fixture " + run[:8],
	})
	require.NoError(t, err)
	fx.contactID = contact.ID

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// These three take LIKE patterns, so the trailing % is what makes them
		// prefix deletes rather than exact-match no-ops.
		_ = chain.messaging.CommsMessageRepo.HardDeleteBySourceAndExternalIDPrefix(cleanupCtx, repository.InteractionSourceWhatsApp, fx.idPrefix+"%")
		_ = chain.core.Interaction.HardDeleteInteractionsBySourceRefPrefix(cleanupCtx, repository.InteractionSourceWhatsApp, fx.refPrefix+"%")
		_ = chain.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(cleanupCtx, repository.InteractionSourceWhatsApp, fx.refPrefix+"%")
		_ = chain.core.Contact.HardDeleteContact(cleanupCtx, contact.ID)
	})

	now := accelerated.GetCurrentTime()
	stage := func(chatJID, suffix string, sentAt time.Time) *repository.CommsMessage {
		t.Helper()
		body := "wiring fixture"
		peer := chatJID
		row, err := chain.messaging.CommsMessageRepo.UpsertChatMessage(ctx, repository.UpsertChatMessageParams{
			Source:           repository.InteractionSourceWhatsApp,
			ExternalID:       fx.idPrefix + suffix,
			ThreadID:         chatJID,
			Body:             &body,
			PeerHandle:       &peer,
			Direction:        repository.InteractionDirectionInbound,
			SentAt:           sentAt,
			MatchedContactID: &contact.ID,
		})
		require.NoError(t, err)
		return row
	}

	// Chat C: two rows inside the burst window → one interaction (assertion 1).
	fx.firstC = fx.idPrefix + "c1"
	stage(fx.chatC, "c1", now.Add(-40*time.Minute))
	stage(fx.chatC, "c2", now.Add(-30*time.Minute))
	// Chat D: one row the reenqueuer's synchronous pass must find (assertion 4).
	fx.firstD = fx.idPrefix + "d1"
	stage(fx.chatD, "d1", now.Add(-20*time.Minute))
	// Chat E: one row left UNCLAIMED for assertions 2 and 3.
	fx.rowE = stage(fx.chatE, "e1", now.Add(-10*time.Minute)).ID
	// Chat G: a GROUP-threaded row, also unclaimed, so assertion 3 can prove the
	// reader DISCRIMINATES dm from group_chat rather than merely resolving.
	fx.rowG = stage(fx.chatG, "g1", now.Add(-5*time.Minute)).ID

	return fx
}

// TestWhatsAppWiring_ProductionRegistriesDispatchWhatsApp drives the real wire
// functions and asserts that all seven remaining source-keyed sites dispatch
// for 'whatsapp'. (Site 8, consumer.messageInteractionSources, landed with the
// schema PR and is unexported; its regression guard is the pipeline test in
// backend/tests, where a missing entry makes the recorder reject the source.)
//
// spec: WHA-040
func TestWhatsAppWiring_ProductionRegistriesDispatchWhatsApp(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()

	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	cfg.Database.MigrationsPath = migrationsPathForTest()
	cfg.Features.EnableWhatsAppSync = true

	chain := buildWireChainForGolden(t, cfg)
	fx := seedWhatsAppWiringFixture(t, ctx, chain)

	// --- Sites 3 + 6, behaviorally -----------------------------------------
	// The engine registered under the whatsapp key in the ChatAwareAggregator
	// map must claim chat C's rows and publish the interaction event in ONE
	// transaction, so the event row is durable, synchronous evidence — no
	// running worker required.
	//
	// This discriminates BOTH mis-bindings: a gchat engine under the whatsapp
	// key lists source='gchat' rows, finds none and publishes nothing; an
	// engine built from the gchat source adapter publishes under source
	// 'gchat' with a 'gchat:' ref, so the lookup misses.
	engine := chain.wiring.Engines[repository.InteractionSourceWhatsApp]
	require.NotNil(t, engine, "site 6: no ChatAwareAggregator registered for whatsapp")
	require.NoError(t, engine.AggregateForContact(ctx, fx.contactID, fx.chatC))

	wantRefC := repository.InteractionSourceWhatsApp + ":" + fx.chatC + ":" + fx.firstC + ":" + fx.contactID.String()
	envC, err := chain.eventRepo.FindEventBySource(ctx, repository.InteractionSourceWhatsApp, wantRefC)
	require.NoError(t, err, "sites 3+6: the whatsapp engine published no event for %s", wantRefC)
	require.NotNil(t, envC)

	// --- Site 1: the staging-processor registry ----------------------------
	// An unregistered source returns 0 rows affected (and a logged warning),
	// which is exactly what makes a missing entry silent in production.
	//
	// comms_message.interaction_id REFERENCES interaction(id), so an arbitrary
	// UUID fails the FK at statement time even inside a rolled-back tx: create
	// a real interaction first.
	markRef := "wiring-session-" + uuid.NewString()
	desc := "wiring fixture interaction"
	sourceRef := fx.refPrefix + "3@s.whatsapp.net:" + fx.idPrefix + "e1"
	interaction, err := chain.core.Interaction.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:   fx.contactID,
		Source:      repository.InteractionSourceWhatsApp,
		SourceRef:   &sourceRef,
		OccurredAt:  accelerated.GetCurrentTime(),
		Description: &desc,
		Direction:   repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	tx, err := chain.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	affected, err := chain.messaging.StagingRegistry.MarkProcessedTx(
		ctx, tx, repository.InteractionSourceWhatsApp, []uuid.UUID{fx.rowE}, interaction.ID, markRef,
	)
	require.NoError(t, err)
	assert.EqualValues(t, 1, affected, "site 1: no staging processor registered for whatsapp")

	// --- Site 2: the venue-container-reader registry ------------------------
	// An unregistered source returns (nil, nil) — the recorder then writes a
	// NULL venue_id and nothing complains.
	//
	// Resolving BOTH a direct-chat row and a group row, and asserting the kinds
	// DIFFER, is what makes this more than "a venue came back": the reader this
	// PR adds is a copy of GChat's, whose kind is hard-coded to group_chat —
	// right for Chat spaces, wrong for WhatsApp. A single-row assertion stays
	// green under that defect.
	dmVenueID, err := chain.messaging.VenueResolver.ResolveMessageVenueTx(
		ctx, tx, repository.InteractionSourceWhatsApp, []uuid.UUID{fx.rowE},
	)
	require.NoError(t, err)
	require.NotNil(t, dmVenueID, "site 2: no venue container reader registered for whatsapp")

	groupVenueID, err := chain.messaging.VenueResolver.ResolveMessageVenueTx(
		ctx, tx, repository.InteractionSourceWhatsApp, []uuid.UUID{fx.rowG},
	)
	require.NoError(t, err)
	require.NotNil(t, groupVenueID)

	venueRepoTx := repository.NewVenueRepository(db.New(tx))
	dmVenue, err := venueRepoTx.GetVenue(ctx, *dmVenueID)
	require.NoError(t, err)
	groupVenue, err := venueRepoTx.GetVenue(ctx, *groupVenueID)
	require.NoError(t, err)
	assert.Equal(t, repository.VenueKindDM, dmVenue.Kind, "site 2: a one-to-one chat JID must resolve to a dm venue")
	assert.Equal(t, repository.VenueKindGroupChat, groupVenue.Kind, "site 2: an @g.us chat JID must resolve to a group_chat venue")

	require.NoError(t, tx.Rollback(ctx))

	// --- Site 4: the aggregator-reenqueuer registry -------------------------
	// The reenqueuer's SYNCHRONOUS second action parses the chat id by
	// stripping "<source>:" from the envelope PeerRef, so an entry constructed
	// with InteractionSourceGChat cannot parse a "whatsapp:" PeerRef, aggregates
	// nothing and publishes nothing. (The River Insert it also performs is not
	// asserted: reading river_job would need raw SQL.)
	reenqueuer := chain.agg.ReenqueuerEntries[repository.InteractionSourceWhatsApp]
	require.NotNil(t, reenqueuer, "site 4: no aggregator reenqueuer registered for whatsapp")
	envD := envelopeWithPeerRef(t, repository.InteractionSourceWhatsApp+":"+fx.chatD)
	require.NoError(t, reenqueuer.Reenqueue(ctx, envD, fx.contactID))

	wantRefD := repository.InteractionSourceWhatsApp + ":" + fx.chatD + ":" + fx.firstD + ":" + fx.contactID.String()
	envDOut, err := chain.eventRepo.FindEventBySource(ctx, repository.InteractionSourceWhatsApp, wantRefD)
	require.NoError(t, err, "site 4: the whatsapp reenqueuer aggregated nothing for %s", wantRefD)
	require.NotNil(t, envDOut)

	// --- Site 5: the per-source chat-lister registry ------------------------
	// An unregistered source returns (nil, nil); a lister bound to another
	// source returns rows for that source, i.e. none of ours.
	chats, err := chain.wiring.ChatListers.ListUnprocessedChats(ctx, repository.InteractionSourceWhatsApp, fx.contactID)
	require.NoError(t, err)
	assert.Contains(t, chats, fx.chatE, "site 5: chat lister did not enumerate the whatsapp chat")

	// --- Site 7: the sweeper-lister map -------------------------------------
	sweeper := chain.wiring.SweeperListers[repository.InteractionSourceWhatsApp]
	require.NotNil(t, sweeper, "site 7: no sweeper lister registered for whatsapp")
	contactIDs, err := sweeper.ListUnprocessedContactIDs(ctx)
	require.NoError(t, err)
	assert.Contains(t, contactIDs, fx.contactID, "site 7: sweeper lister did not enumerate the contact")
}

// envelopeWithPeerRef builds the message.received envelope a comms aggregator
// emits, carrying the PeerRef the reenqueuer parses its chat scope from.
func envelopeWithPeerRef(t *testing.T, peerRef string) *events.Envelope {
	t.Helper()
	payload, err := json.Marshal(events.MessageReceivedPayload{
		Version:   1,
		PeerRef:   peerRef,
		MessageAt: accelerated.GetCurrentTime(),
	})
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     repository.InteractionSourceWhatsApp,
		SourceID:   uuid.NewString(),
		Kind:       events.KindMessageReceived,
		Payload:    payload,
		ObservedAt: accelerated.GetCurrentTime(),
	}
}

// TestWhatsAppWiring_RematchHandlersFollowTheFeatureFlag pins D5.3, the ONE
// piece of this PR's wiring that is not inert when WhatsApp is off.
//
// The engine and the seven registries are deliberately unconditional (rows
// staged under an earlier enabled boot must still aggregate after a restart
// with the flag off), and every query they run is source='whatsapp'-scoped, so
// they cost nothing. The rematch handlers are different: registering the
// 'phone' handler unconditionally would make a bare phone method
// rematch-eligible on a deployment with BOTH Telegram and WhatsApp disabled,
// minting a rematch job where none exists today.
//
// Nothing else observes this. The golden lists pin worker/periodic/provider
// names, not rematch registrations, so hoisting the two Register calls out of
// the `if` would be caught by no test without this one.
func TestWhatsAppWiring_RematchHandlersFollowTheFeatureFlag(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// A phone method is the discriminating input: 'whatsapp' has no other
	// handler, but 'phone' is shared with Telegram, so an unconditional
	// registration changes eligibility on a deployment that has neither.
	phone := []service.Method{{Type: string(repository.ContactMethodPhone), Value: "+12045550101"}}
	whatsapp := []service.Method{{Type: string(repository.ContactMethodWhatsApp), Value: "+12045550101"}}

	for _, tc := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{name: "off", enabled: false, want: 0},
		{name: "on", enabled: true, want: 1},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cloneURL, drop := testdb.NewEphemeralClone(t)
			t.Cleanup(drop)

			cfg := config.TestConfig()
			cfg.Database.URL = cloneURL
			cfg.Database.MigrationsPath = migrationsPathForTest()
			cfg.Features.EnableWhatsAppSync = tc.enabled

			chain := buildWireChainForGolden(t, cfg)

			assert.Len(t, chain.graph.RematchService.EligibleMethods(phone), tc.want,
				"a phone method must be rematch-eligible only while WhatsApp sync is enabled (Telegram is off in this chain)")
			assert.Len(t, chain.graph.RematchService.EligibleMethods(whatsapp), tc.want,
				"a whatsapp method must be rematch-eligible only while WhatsApp sync is enabled")

			if !tc.enabled {
				return
			}

			// With the handlers registered, DISPATCH one. This is the only
			// assertion in the suite that exercises a registered rematch handler
			// end-to-end, and it is what makes run()'s
			// CommsMessageRepo.SetPool(database.Pool) observable:
			// AttachUnmatchedByPeer owns its own transaction and returns
			// "repository has no pool (call SetPool)" without it, so a chain that
			// silently drifted from run()'s wiring fails here rather than in
			// production. No rows match the number, so the handler attaches
			// nothing and reports zero.
			contact, err := chain.core.Contact.CreateContact(context.Background(), repository.CreateContactRequest{
				FullName: "WhatsApp Flag Gate " + uuid.NewString()[:8],
			})
			require.NoError(t, err)
			require.NoError(t,
				chain.graph.RematchService.Run(context.Background(), uuid.New(), contact.ID, phone),
				"the registered whatsapp phone handler must run against a pooled comms repository")
		})
	}
}
