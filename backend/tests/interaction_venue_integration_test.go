//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/testdb"
	"personal-crm/backend/tests/testsupport"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// interactionVenueVersion is the golang-migrate version of the venue-model
// migration (069). The down/up round-trip positions the clone here first so
// Steps(-1) rolls down 069 specifically, robust to later migrations landing
// above it.
const interactionVenueVersion = 69

// TestInteractionVenue_LivePath drives the REAL recorder pipeline (telegram)
// and asserts the live ResolveVenueForInteraction path sets venue_id, that two
// messages in the SAME chat share ONE venue node, and that adding venue_id does
// not perturb the contact's cadence columns (the headline-risk regression).
func TestInteractionVenue_LivePath(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// First telegram message in the chat → matched interaction with a venue.
	tgMsg1 := gen.TelegramMessage(spec, factory.MatchSeeded)
	res1, err := h.ReplayTelegram(ctx, contact.ID, tgMsg1)
	require.NoError(t, err)
	require.True(t, res1.Matched)

	// Snapshot cadence columns AFTER the first interaction settled but BEFORE the
	// second — the venue is already populated, so a second same-chat interaction
	// must reuse the node and must not perturb cadence beyond the normal
	// interaction effect.
	afterFirst, err := h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)

	// Second message in the SAME chat (reuse the peer/chat id, bump the message
	// id) so the two interactions share one venue container.
	tgMsg2 := tgMsg1
	tgMsg2.TelegramMessageID = tgMsg1.TelegramMessageID + 1
	res2, err := h.ReplayTelegram(ctx, contact.ID, tgMsg2)
	require.NoError(t, err)
	require.True(t, res2.Matched)

	// Both telegram interactions resolve to the SAME venue node (one container).
	venueIDs := distinctVenueNodeIDs(t, ctx, h, contact.ID, repository.InteractionSourceTelegram)
	require.Len(t, venueIDs, 1, "two messages in one chat must share exactly one venue node")

	// The venue node is a live telegram dm/group_chat venue.
	venue, err := h.VenueRepo().GetVenue(ctx, venueIDs[0])
	require.NoError(t, err)
	require.Equal(t, repository.InteractionSourceTelegram, venue.Source)
	assert.Contains(t, []string{repository.VenueKindDM, repository.VenueKindGroupChat}, venue.Kind)

	// Cadence regression: the venue resolution must not regress the cadence math.
	// last_contacted advances only by the (normal) second-interaction effect, and
	// the venue write touches no cadence column — assert the cadence fields move
	// only as a normal second interaction would (never NULL-ed, never reset).
	afterSecond, err := h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, afterSecond.LastInteractionAt, "venue write must not null last_interaction_at")
	if afterFirst.LastInteractionAt != nil {
		require.False(t, afterSecond.LastInteractionAt.Before(*afterFirst.LastInteractionAt),
			"last_interaction_at must not move backward across the venue-bearing second interaction")
	}
}

// TestInteractionVenue_Backfill seeds pre-existing venue-less interactions across
// the distinct backfill JOIN SHAPES and asserts each resolves to the expected
// venue. The three shapes are: the content interaction_id FK (telegram here;
// email/gchat/messages/phone share this exact shape, differing only in the
// column read + the kind), the gcal source_ref = calendar_event.id::text join,
// and the anarlog split_part(source_ref) extraction + the gcal-meeting-venue
// reuse. Also covered: two same-chat telegram rows sharing one node, the
// anarlog→gcal venue reuse, the unlinked-anarlog session venue, the NULL-venue
// manual case, in-place idempotent re-run, and that the backfill node id matches
// the live helper's deterministic id. (The LIVE recorder path for every source —
// email/messages/gchat/telegram/gcal — is covered by the synthetic replay
// adapters' assertContactVenue assertions in TestSyntheticReplay_*.)
func TestInteractionVenue_Backfill(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	// Migration-subject test: rolls the schema down, so it stays serial and uses
	// an isolated clone (never the shared package DB).

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	migrationsPath := getMigrationsPath()

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	contactRepo := repository.NewContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	meetingNoteRepo := repository.NewMeetingNoteRepository(database.Queries)
	venueRepo := repository.NewVenueRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)

	now := accelerated.GetCurrentTime().UTC()

	// A contact + its person node (the interaction FK target). Seed it with a
	// populated cadence + last_contacted so the byte-identical cadence assertion
	// below is a non-trivial check (a mostly-NULL contact would pass vacuously).
	cadence := "weekly"
	lastContacted := now.Add(-72 * time.Hour)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Venue Backfill Subject",
		Cadence:       &cadence,
		LastContacted: &lastContacted,
	})
	require.NoError(t, err)
	require.NotNil(t, contact.LastContacted, "precondition: seeded contact has a populated cadence state")

	// --- telegram: two messages in ONE chat (must share one venue node) ---
	tgChatID := int64(778899)
	tg1 := uuid.New()
	tg2 := uuid.New()
	seedTelegramInteraction(ctx, t, interactionRepo, database.Queries, tg1, contact.ID, tgChatID, 1, now)
	seedTelegramInteraction(ctx, t, interactionRepo, database.Queries, tg2, contact.ID, tgChatID, 2, now)
	tgContainer := fmt.Sprintf("%d", tgChatID)

	// --- gcal: an event + an interaction joined by source_ref = calendar_event.id ---
	event, err := calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:     "evt-backfill-1",
		GcalCalendarID:  "primary",
		GoogleAccountID: "acct-backfill-1",
		StartTime:       now,
		EndTime:         now.Add(time.Hour),
		Status:          "confirmed",
		SyncedAt:        now,
	})
	require.NoError(t, err)
	gcalID := uuid.New()
	gcalRef := event.ID.String()
	_, err = interactionRepo.TestInsertInteraction(ctx, gcalID, contact.ID, repository.InteractionSourceGCal, &gcalRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)
	gcalContainer := repository.GCalVenueContainerID(event.GcalEventID, event.GcalCalendarID, event.GoogleAccountID)

	// --- anarlog linked to the gcal event: must REUSE the gcal meeting venue ---
	linkedKind := repository.LinkedKindEvent
	linkedSession := uuid.New()
	mnTx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	_, err = meetingNoteRepo.InsertMeetingNoteTx(ctx, mnTx, repository.InsertMeetingNoteParams{
		AnarlogSessionID: linkedSession,
		LinkedKind:       &linkedKind,
		LinkedID:         &event.ID,
		LinkageState:     repository.LinkageStateLinked,
		MeetingAt:        now,
	})
	require.NoError(t, err)
	require.NoError(t, mnTx.Commit(ctx))
	anarlogLinkedID := uuid.New()
	anarlogLinkedRef := fmt.Sprintf("anarlog:%s:%s", linkedSession, contact.ID)
	_, err = interactionRepo.TestInsertInteraction(ctx, anarlogLinkedID, contact.ID, repository.InteractionSourceAnarlogSessions, &anarlogLinkedRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)

	// --- anarlog UNLINKED: gets its own session venue ---
	unlinkedSession := uuid.New()
	anarlogUnlinkedID := uuid.New()
	anarlogUnlinkedRef := fmt.Sprintf("anarlog:%s:%s", unlinkedSession, contact.ID)
	_, err = interactionRepo.TestInsertInteraction(ctx, anarlogUnlinkedID, contact.ID, repository.InteractionSourceAnarlogSessions, &anarlogUnlinkedRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)

	// --- messages: group chat keyed on chat_guid (content interaction_id FK) ---
	messagesID := uuid.New()
	messagesContainer := "iMessage;+;chat-guid-backfill"
	mRef := "msg-ix"
	_, err = interactionRepo.TestInsertInteraction(ctx, messagesID, contact.ID, repository.InteractionSourceMessages, &mRef, now, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	_, err = database.Queries.TestInsertMessagesMessageLinked(ctx, db.TestInsertMessagesMessageLinkedParams{
		Guid: "imsg-guid-1", ChatGuid: messagesContainer, PeerHandle: "+15551110000",
		SentAt: pgtype.Timestamptz{Time: now, Valid: true}, IsOutgoing: false, IsGroupChat: true,
		InteractionID: pgtype.UUID{Bytes: messagesID, Valid: true},
	})
	require.NoError(t, err)

	// --- email: thread keyed on comms_message.thread_id (content FK) ---
	emailID := uuid.New()
	emailContainer := "THREAD-backfill-1"
	eRef := "email-ix"
	_, err = interactionRepo.TestInsertInteraction(ctx, emailID, contact.ID, repository.InteractionSourceEmail, &eRef, now, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	_, err = database.Queries.TestInsertCommsMessageLinked(ctx, db.TestInsertCommsMessageLinkedParams{
		Source: repository.InteractionSourceEmail, ExternalID: "msgid-backfill-1",
		ThreadID: pgtype.Text{String: emailContainer, Valid: true}, Direction: "inbound",
		SentAt:           pgtype.Timestamptz{Time: now, Valid: true},
		MatchedContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
		InteractionID:    pgtype.UUID{Bytes: emailID, Valid: true},
	})
	require.NoError(t, err)

	// --- gchat: group_chat keyed on comms_message.thread_id (space resource) ---
	gchatID := uuid.New()
	gchatContainer := "spaces/AAAAbackfill"
	gcRef := "gchat-ix"
	_, err = interactionRepo.TestInsertInteraction(ctx, gchatID, contact.ID, repository.InteractionSourceGChat, &gcRef, now, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	_, err = database.Queries.TestInsertCommsMessageLinked(ctx, db.TestInsertCommsMessageLinkedParams{
		Source: repository.InteractionSourceGChat, ExternalID: "gcmsg-backfill-1",
		ThreadID: pgtype.Text{String: gchatContainer, Valid: true}, Direction: "inbound",
		SentAt:           pgtype.Timestamptz{Time: now, Valid: true},
		MatchedContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
		InteractionID:    pgtype.UUID{Bytes: gchatID, Valid: true},
	})
	require.NoError(t, err)

	// --- phone: call keyed on phone_call.call_unique_id (content FK) ---
	phoneID := uuid.New()
	phoneContainer := "call-uid-backfill-1"
	pRef := "phone-ix"
	_, err = interactionRepo.TestInsertInteraction(ctx, phoneID, contact.ID, repository.InteractionSourcePhoneCalls, &pRef, now, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	_, err = database.Queries.TestInsertPhoneCallLinked(ctx, db.TestInsertPhoneCallLinkedParams{
		CallUniqueID: phoneContainer, PeerHandle: "+15551110000", PeerNormalized: "+15551110000",
		Service: "voice", Direction: "inbound", DurationSeconds: 60,
		StartedAt:        pgtype.Timestamptz{Time: now, Valid: true},
		MatchedContactID: pgtype.UUID{Bytes: contact.ID, Valid: true},
		InteractionID:    pgtype.UUID{Bytes: phoneID, Valid: true},
	})
	require.NoError(t, err)

	// --- manual: no container → venue_id stays NULL ---
	manualID := uuid.New()
	_, err = interactionRepo.TestInsertInteraction(ctx, manualID, contact.ID, repository.InteractionSourceManual, nil, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)

	// Cadence regression (the headline risk): snapshot the contact's cadence
	// columns BEFORE the backfill so we can prove the backfill touches NO cadence
	// state — it only populates interaction.venue_id.
	beforeBackfill, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)

	// Run the backfill by rolling 069 down then up on the clone.
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(interactionVenueVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone at the venue-model tip")
	}
	require.NoError(t, m.Steps(-1), "roll the venue-model migration down")
	require.NoError(t, m.Steps(1), "re-apply the venue model (runs the backfill over the seeded rows)")

	// Cadence columns are byte-identical after the backfill (it never touches the
	// contact row).
	afterBackfill, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeBackfill.LastContacted, afterBackfill.LastContacted, "backfill must not touch last_contacted")
	assert.Equal(t, beforeBackfill.LastInteractionAt, afterBackfill.LastInteractionAt, "backfill must not touch last_interaction_at")
	assert.Equal(t, beforeBackfill.LastOutreachAt, afterBackfill.LastOutreachAt, "backfill must not touch last_outreach_at")
	assert.Equal(t, beforeBackfill.LastResponseAt, afterBackfill.LastResponseAt, "backfill must not touch last_response_at")
	assert.Equal(t, beforeBackfill.ContactBy, afterBackfill.ContactBy, "backfill must not touch contact_by")

	// Recompute leg: run a CadenceUpdater apply against the now-venue-bearing
	// interactions and assert the cadence columns are STILL byte-identical (proves
	// the cadence recompute path tolerates venue-bearing rows). The cadence
	// machinery partitions purely by contact_id and
	// never reads venue_id, so applying a stale (older-than-last_contacted)
	// telegram interaction is a forward-only no-op that must leave cadence
	// untouched — proving the recompute path is unperturbed by the new column.
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false)
	recomputeTx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, cadenceUpdater.ApplyInteraction(ctx, recomputeTx, repository.ApplyInteractionRequest{
		ContactID:  contact.ID,
		Direction:  repository.InteractionDirectionInbound,
		Source:     repository.InteractionSourceTelegram,
		OccurredAt: lastContacted.Add(-24 * time.Hour), // older than last_contacted → forward-only no-op
	}))
	require.NoError(t, recomputeTx.Commit(ctx))
	afterRecompute, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, afterBackfill.LastContacted, afterRecompute.LastContacted, "cadence recompute must be identical post-backfill")
	assert.Equal(t, afterBackfill.ContactBy, afterRecompute.ContactBy, "cadence recompute must be identical post-backfill")

	// gcal interaction resolves to a meeting venue keyed on the 3-tuple.
	gcalVenueID := requireInteractionVenue(t, ctx, interactionRepo, venueRepo, gcalID, repository.InteractionSourceGCal, repository.VenueKindMeeting, gcalContainer)

	// telegram: both rows resolve to the SAME venue node (one container).
	tg1Venue := requireInteractionVenue(t, ctx, interactionRepo, venueRepo, tg1, repository.InteractionSourceTelegram, repository.VenueKindDM, tgContainer)
	tg2Venue := requireInteractionVenue(t, ctx, interactionRepo, venueRepo, tg2, repository.InteractionSourceTelegram, repository.VenueKindDM, tgContainer)
	require.Equal(t, tg1Venue, tg2Venue, "two messages in one chat must share one venue node")

	// The backfill node id matches the live helper's deterministic id (they must
	// stay in sync so live recorders and the backfill converge on one node).
	require.Equal(t, repository.VenueNodeID(repository.InteractionSourceTelegram, repository.VenueKindDM, tgContainer), tg1Venue,
		"backfill venue node id must equal the live helper's deterministic id")

	// anarlog LINKED reuses the gcal meeting venue (the only cross-source merge).
	linkedInteraction, err := interactionRepo.GetInteraction(ctx, anarlogLinkedID)
	require.NoError(t, err)
	require.NotNil(t, linkedInteraction.VenueID)
	require.Equal(t, gcalVenueID, *linkedInteraction.VenueID, "anarlog linked to a gcal event must reuse the gcal meeting venue")

	// anarlog UNLINKED gets its own session venue.
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, anarlogUnlinkedID,
		repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, unlinkedSession.String())

	// messages / email / gchat / phone each resolve to their expected venue
	// (the content interaction_id FK join shapes).
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, messagesID,
		repository.InteractionSourceMessages, repository.VenueKindGroupChat, messagesContainer)
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, emailID,
		repository.InteractionSourceEmail, repository.VenueKindEmailThread, emailContainer)
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, gchatID,
		repository.InteractionSourceGChat, repository.VenueKindGroupChat, gchatContainer)
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, phoneID,
		repository.InteractionSourcePhoneCalls, repository.VenueKindCall, phoneContainer)

	// No orphan venue nodes: every venue-type node is referenced by at least one
	// interaction (proves the WHERE NOT EXISTS node-mint guard).
	require.Zero(t, countOrphanVenueNodes(ctx, t, nodeRepo), "backfill must leave no orphan venue nodes")

	// manual interaction has NO venue.
	manualInteraction, err := interactionRepo.GetInteraction(ctx, manualID)
	require.NoError(t, err)
	require.Nil(t, manualInteraction.VenueID, "manual interaction must have a NULL venue_id")

	// In-place idempotency: re-execute the up migration's backfill SQL directly
	// (no down) and assert it is a true no-op — the venue node count and every
	// venue_id are unchanged (proves WHERE venue_id IS NULL + the ON CONFLICT
	// guards, not just down/up symmetry).
	venueNodesBefore := countVenueNodes(ctx, t, nodeRepo)
	upSQL, readErr := os.ReadFile(migrationsPath + "/069_interaction_venue.up.sql")
	require.NoError(t, readErr)
	_, err = database.Pool.Exec(ctx, string(upSQL))
	require.NoError(t, err, "re-running the up migration backfill in-place must succeed")
	require.Equal(t, venueNodesBefore, countVenueNodes(ctx, t, nodeRepo), "in-place re-run must not create new venue nodes")
	tg1VenueAgain := requireInteractionVenue(t, ctx, interactionRepo, venueRepo, tg1, repository.InteractionSourceTelegram, repository.VenueKindDM, tgContainer)
	require.Equal(t, tg1Venue, tg1VenueAgain, "in-place re-run must leave venue_id unchanged")
}

// countVenueNodes returns the number of venue-type nodes (via the test-only
// sqlc count, not raw SQL).
func countVenueNodes(ctx context.Context, t *testing.T, nodeRepo *repository.NodeRepository) int64 {
	t.Helper()
	n, err := nodeRepo.TestCountVenueNodes(ctx)
	require.NoError(t, err)
	return n
}

// countOrphanVenueNodes returns the number of venue-type nodes that no live
// interaction references via venue_id (via the test-only sqlc count).
func countOrphanVenueNodes(ctx context.Context, t *testing.T, nodeRepo *repository.NodeRepository) int64 {
	t.Helper()
	n, err := nodeRepo.TestCountOrphanVenueNodes(ctx)
	require.NoError(t, err)
	return n
}

// TestInteractionVenue_MigrationAtomicity proves the REAL 069 up migration is
// explicitly transactional: it runs the actual 069_interaction_venue.up.sql
// content (with a deterministic error injected just before its own COMMIT) and
// asserts venue_id did NOT survive. Because the injected error is placed inside
// the real file's own BEGIN..COMMIT boundary, the test FAILS if a future edit
// strips BEGIN;/COMMIT; from the real file (the ADD COLUMN would then commit
// before the error). The companion guard-skip case below proves the actual file
// also tolerates a malformed anarlog source_ref without aborting.
func TestInteractionVenue_MigrationAtomicity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	migrationsPath := getMigrationsPath()

	// Roll the venue model down so the column is absent.
	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(interactionVenueVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err)
	}
	require.NoError(t, m.Steps(-1), "roll the venue model down so venue_id is absent")
	require.False(t, interactionVenueColumnExists(ctx, t, database), "precondition: venue_id absent after down")

	// Take the REAL 069 up SQL and inject a deterministic error immediately
	// before its OWN final COMMIT; — so the whole real file (its BEGIN, its DDL,
	// its backfill) runs and then errors while still inside the file's own
	// transaction. If BEGIN;/COMMIT; are present (they are), the ADD COLUMN rolls
	// back. If a future edit removes them, the ADD COLUMN auto-commits before the
	// error and this assertion fails — which is exactly the regression we guard.
	realUp, readErr := os.ReadFile(migrationsPath + "/069_interaction_venue.up.sql")
	require.NoError(t, readErr)
	brokenReal := injectErrorBeforeFinalCommit(t, string(realUp))
	_, execErr := database.Pool.Exec(ctx, brokenReal)
	require.Error(t, execErr, "the error injected into the real migration must fail it")
	require.False(t, interactionVenueColumnExists(ctx, t, database),
		"venue_id must NOT survive an error inside the real file's BEGIN..COMMIT (proves it is explicitly transactional)")

	// Re-apply the real migration cleanly so teardown is on a consistent schema.
	require.NoError(t, m.Steps(1), "re-apply the real venue model")
	require.True(t, interactionVenueColumnExists(ctx, t, database))
}

// TestInteractionVenue_MalformedAnarlogRefIsSkipped runs the REAL 069 migration
// over a malformed anarlog interaction (source_ref segment-2 is NOT a UUID) and
// asserts the migration SUCCEEDS. Step-1's anarlog→gcal reuse compares the
// trusted anarlog_session_id::text against the raw source_ref segment (it never
// casts the untrusted segment to uuid), so a malformed ref cannot abort the
// whole BEGIN..COMMIT. A linked well-formed anarlog row in the same run reuses
// the gcal meeting venue (forcing Step-1 to actually evaluate). This test FAILS
// if the comparison reverts to split_part(...)::uuid (the cast then raises
// invalid-uuid and aborts the migration) — mutation-verified.
func TestInteractionVenue_MalformedAnarlogRefIsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	contactRepo := repository.NewContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	meetingNoteRepo := repository.NewMeetingNoteRepository(database.Queries)
	venueRepo := repository.NewVenueRepository(database.Queries)
	now := accelerated.GetCurrentTime().UTC()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Malformed Anarlog Subject"})
	require.NoError(t, err)

	// A gcal event + a LINKED meeting_note + a well-formed anarlog interaction for
	// that session. This FORCES the Step-1 anarlog→gcal join to evaluate (its
	// meeting_note ⋈ calendar_event ⋈ venue chain produces a row), so the
	// untrusted-source_ref comparison in Step-1 actually runs — without this setup
	// the cast-safety guard would never be exercised by the test.
	event, err := calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID: "evt-malformed-1", GcalCalendarID: "primary", GoogleAccountID: "acct-malformed-1",
		StartTime: now, EndTime: now.Add(time.Hour), Status: "confirmed", SyncedAt: now,
	})
	require.NoError(t, err)
	// A gcal INTERACTION for the event so the gcal backfill block (which runs
	// before anarlog) mints the meeting venue the anarlog reuse then finds.
	gcalIxID := uuid.New()
	gcalRef := event.ID.String()
	_, err = interactionRepo.TestInsertInteraction(ctx, gcalIxID, contact.ID, repository.InteractionSourceGCal, &gcalRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)
	linkedKind := repository.LinkedKindEvent
	linkedSession := uuid.New()
	mnTx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	_, err = meetingNoteRepo.InsertMeetingNoteTx(ctx, mnTx, repository.InsertMeetingNoteParams{
		AnarlogSessionID: linkedSession, LinkedKind: &linkedKind, LinkedID: &event.ID,
		LinkageState: repository.LinkageStateLinked, MeetingAt: now,
	})
	require.NoError(t, err)
	require.NoError(t, mnTx.Commit(ctx))
	linkedID := uuid.New()
	linkedRef := fmt.Sprintf("anarlog:%s:%s", linkedSession, contact.ID)
	_, err = interactionRepo.TestInsertInteraction(ctx, linkedID, contact.ID, repository.InteractionSourceAnarlogSessions, &linkedRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)

	// A MALFORMED anarlog ref (segment 2 is not a UUID) present in the SAME run.
	// Step-1 compares anarlog_session_id::text against this raw segment; the
	// migration must NOT raise invalid-uuid on it (it can't — there is no cast of
	// the untrusted text). If a future edit reverts to split_part(...)::uuid this
	// row makes the whole BEGIN..COMMIT migration abort and the test fails.
	badID := uuid.New()
	badRef := fmt.Sprintf("anarlog:not-a-uuid:%s", contact.ID)
	_, err = interactionRepo.TestInsertInteraction(ctx, badID, contact.ID, repository.InteractionSourceAnarlogSessions, &badRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)

	// A well-formed UNLINKED anarlog ref → its own session venue.
	goodSession := uuid.New()
	goodID := uuid.New()
	goodRef := fmt.Sprintf("anarlog:%s:%s", goodSession, contact.ID)
	_, err = interactionRepo.TestInsertInteraction(ctx, goodID, contact.ID, repository.InteractionSourceAnarlogSessions, &goodRef, now, repository.InteractionDirectionMutual)
	require.NoError(t, err)

	// Run the REAL 069 by rolling it down then up over the seeded rows.
	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(interactionVenueVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err)
	}
	require.NoError(t, m.Steps(-1), "roll the venue model down")
	require.NoError(t, m.Steps(1), "the REAL backfill must SUCCEED despite the malformed anarlog ref present")

	// Step-1 DID fire: the linked anarlog interaction reuses the gcal MEETING
	// venue (not a session venue) — proving the cast-safe comparison still matches
	// a well-formed session.
	gcalContainer := repository.GCalVenueContainerID(event.GcalEventID, event.GcalCalendarID, event.GoogleAccountID)
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, linkedID,
		repository.InteractionSourceGCal, repository.VenueKindMeeting, gcalContainer)

	// The well-formed UNLINKED row resolves to its session venue (Step 2).
	requireInteractionVenue(t, ctx, interactionRepo, venueRepo, goodID,
		repository.InteractionSourceAnarlogSessions, repository.VenueKindSession, goodSession.String())

	// The malformed row did NOT abort the migration: Step-2 (text-based) gives it
	// a session venue keyed on the raw segment-2 text.
	badInteraction, err := interactionRepo.GetInteraction(ctx, badID)
	require.NoError(t, err)
	require.NotNil(t, badInteraction.VenueID, "malformed row gets a session venue from the text-based Step 2")
}

// TestInteractionVenue_DownGuardPreservesReferencedVenue runs the 069 DOWN over
// two venue nodes — one referenced by an assertion, one not — and asserts the
// guarded cleanup deletes ONLY the unreferenced one. This exercises the down
// migration's assertion-reference guard (otherwise only ever run as a bare
// reposition Steps(-1)).
func TestInteractionVenue_DownGuardPreservesReferencedVenue(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	nodeRepo := repository.NewNodeRepository(database.Queries)
	venueRepo := repository.NewVenueRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	now := accelerated.GetCurrentTime().UTC()

	// Two venue nodes via the resolver (clone is at the tip, venue_id present).
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	referencedVenue, err := venueRepo.ResolveVenueForInteraction(ctx, tx, repository.InteractionSourceTelegram, repository.VenueKindDM, "down-guard-referenced", "")
	require.NoError(t, err)
	unreferencedVenue, err := venueRepo.ResolveVenueForInteraction(ctx, tx, repository.InteractionSourceTelegram, repository.VenueKindDM, "down-guard-unreferenced", "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// An accepted assertion whose subject is the referenced venue node. The
	// assertion→node FK is restrict, so the down's guard must keep this node.
	addr := "down-guard-value"
	_, err = assertionRepo.InsertAssertion(ctx, repository.InsertAssertionParams{
		SubjectNodeID:  referencedVenue,
		PredicateKey:   "home_address",
		ValueText:      &addr,
		KnowledgeFrom:  now,
		Confidence:     80,
		Salience:       40,
		Status:         repository.AssertionStatusAccepted,
		PropositionKey: "down-guard-prop-1",
	})
	require.NoError(t, err)

	// Roll 069 DOWN.
	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(interactionVenueVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err)
	}
	require.NoError(t, m.Steps(-1), "roll the venue model down")

	// The assertion-referenced venue node SURVIVES; the unreferenced one is gone.
	_, err = nodeRepo.GetNode(ctx, referencedVenue)
	require.NoError(t, err, "the assertion-referenced venue node must survive the guarded down")
	_, err = nodeRepo.GetNode(ctx, unreferencedVenue)
	require.ErrorIs(t, err, db.ErrNotFound, "the unreferenced venue node must be removed by the down")
}

// --- helpers ---

// injectErrorBeforeFinalCommit returns the migration SQL with a deterministic
// failing statement (SELECT 1/0) inserted immediately before the file's LAST
// COMMIT; line. The error therefore fires INSIDE the file's own transaction —
// so the test is sensitive to whether the real file actually has BEGIN;/COMMIT;
// (strip them and the DDL auto-commits before the error). Fails the test loudly
// if the file has no COMMIT; to anchor against.
func injectErrorBeforeFinalCommit(t *testing.T, sql string) string {
	t.Helper()
	// Require EXACTLY one COMMIT; so the anchor is unambiguous. A future edit that
	// adds a second COMMIT; (in a string literal or a trailing comment) would
	// otherwise shift LastIndex and could land the injected error OUTSIDE the
	// transaction, silently weakening the atomicity proof — fail loudly instead.
	require.Equal(t, 1, strings.Count(sql, "COMMIT;"), "the real migration must contain exactly one COMMIT; to anchor the injected error unambiguously")
	idx := strings.LastIndex(sql, "COMMIT;")
	return sql[:idx] + "SELECT 1/0;\n" + sql[idx:]
}

// seedTelegramInteraction inserts a venue-less interaction plus a telegram_message
// content row linked back to it (private chat → dm venue). Uses the test-only
// generated insert directly (the convention in telegram_message_all_fields_test).
func seedTelegramInteraction(
	ctx context.Context, t *testing.T,
	interactionRepo *repository.InteractionRepository, queries db.Querier,
	interactionID, contactID uuid.UUID, chatID int64, messageID int32, occurredAt time.Time,
) {
	t.Helper()
	ref := fmt.Sprintf("tg:%d:%d", chatID, messageID)
	_, err := interactionRepo.TestInsertInteraction(ctx, interactionID, contactID, repository.InteractionSourceTelegram, &ref, occurredAt, repository.InteractionDirectionInbound)
	require.NoError(t, err)
	ts := pgtype.Timestamptz{Time: occurredAt, Valid: true}
	_, err = queries.InsertFullTelegramMessageForTest(ctx, db.InsertFullTelegramMessageForTestParams{
		TelegramMessageID: messageID,
		TelegramChatID:    chatID,
		ChatType:          "private",
		ChatTitle:         pgtype.Text{String: "DM with Subject", Valid: true},
		MessageType:       "text",
		SentAt:            ts,
		IsOutgoing:        false,
		MatchedContactID:  pgtype.UUID{Bytes: contactID, Valid: true},
		InteractionID:     pgtype.UUID{Bytes: interactionID, Valid: true},
	})
	require.NoError(t, err)
}

// requireInteractionVenue asserts the interaction resolved to a venue with the
// expected (source, kind, container) and returns its node id.
func requireInteractionVenue(
	t *testing.T, ctx context.Context,
	interactionRepo *repository.InteractionRepository, venueRepo *repository.VenueRepository,
	interactionID uuid.UUID, source, kind, container string,
) uuid.UUID {
	t.Helper()
	interaction, err := interactionRepo.GetInteraction(ctx, interactionID)
	require.NoError(t, err)
	require.NotNil(t, interaction.VenueID, "interaction %s must have a venue_id", interactionID)
	venue, err := venueRepo.GetVenue(ctx, *interaction.VenueID)
	require.NoError(t, err)
	assert.Equal(t, source, venue.Source)
	assert.Equal(t, kind, venue.Kind)
	assert.Equal(t, container, venue.SourceContainerID)
	return *interaction.VenueID
}

// distinctVenueNodeIDs returns the distinct venue node ids across a contact's
// interactions of the given source.
func distinctVenueNodeIDs(t *testing.T, ctx context.Context, h *synthetic.Harness, contactID uuid.UUID, source string) []uuid.UUID {
	t.Helper()
	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 100, 0)
	require.NoError(t, err)
	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, r := range rows {
		if r.Source != source || r.VenueID == nil {
			continue
		}
		if _, ok := seen[*r.VenueID]; ok {
			continue
		}
		seen[*r.VenueID] = struct{}{}
		out = append(out, *r.VenueID)
	}
	return out
}

// interactionVenueColumnExists reports whether interaction.venue_id is present.
func interactionVenueColumnExists(ctx context.Context, t *testing.T, database *db.Database) bool {
	t.Helper()
	var exists bool
	err := database.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'interaction' AND column_name = 'venue_id')`).Scan(&exists)
	require.NoError(t, err)
	return exists
}

// whatsappVenueKindFor stages one comms_message(source='whatsapp') row with the
// given chat JID as its thread id, resolves it through the SAME registry the
// recorder uses, and returns the venue's kind. It proves the reader where every
// other VenueContainerReader is proved — against a staged row, through
// ResolveMessageVenueTx — because no test anywhere exercises
// ContainerForMessageTx directly.
func whatsappVenueKindFor(t *testing.T, ctx context.Context, database *db.Database, chatJID, externalID string) string {
	t.Helper()

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	body := "whatsapp venue body"
	peer := chatJID
	row, err := commsRepo.UpsertChatMessage(ctx, repository.UpsertChatMessageParams{
		Source:     repository.InteractionSourceWhatsApp,
		ExternalID: externalID,
		ThreadID:   chatJID,
		Body:       &body,
		PeerHandle: &peer,
		Direction:  repository.InteractionDirectionInbound,
		SentAt:     accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = commsRepo.HardDeleteBySourceAndExternalIDPrefix(context.Background(), repository.InteractionSourceWhatsApp, externalID)
	})

	venueRepo := repository.NewVenueRepository(database.Queries)
	resolver := repository.NewVenueResolverRegistry(
		venueRepo,
		map[string]repository.VenueContainerReader{
			repository.InteractionSourceWhatsApp: repository.NewWhatsAppVenueContainerReader(),
		},
		nil,
	)

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	venueID, err := resolver.ResolveMessageVenueTx(ctx, tx, repository.InteractionSourceWhatsApp, []uuid.UUID{row.ID})
	require.NoError(t, err)
	require.NotNil(t, venueID, "the whatsapp reader must resolve a venue for a staged row")

	venue, err := repository.NewVenueRepository(db.New(tx)).GetVenue(ctx, *venueID)
	require.NoError(t, err)
	require.Equal(t, chatJID, venue.SourceContainerID)
	return venue.Kind
}

// TestWhatsAppVenue_DirectJIDIsDM: a one-to-one chat JID yields a dm venue.
//
// spec: WHA-043.direct-chat-is-a-dm
func TestWhatsAppVenue_DirectJIDIsDM(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	suffix := uuid.NewString()[:8]
	kind := whatsappVenueKindFor(t, ctx, database, "1204555"+suffix+"@s.whatsapp.net", "wa-venue-dm-"+suffix)
	assert.Equal(t, repository.VenueKindDM, kind)
}

// TestWhatsAppVenue_GroupJIDIsGroupChat: a group chat JID yields a group_chat
// venue. This is the case GChat's reader gets wrong as a template — it
// hard-codes group_chat, which is right for spaces and wrong for WhatsApp.
//
// spec: WHA-043.group-chat-is-a-group-chat
func TestWhatsAppVenue_GroupJIDIsGroupChat(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	suffix := uuid.NewString()[:8]
	kind := whatsappVenueKindFor(t, ctx, database, "1204555"+suffix+"-1690000000@g.us", "wa-venue-group-"+suffix)
	assert.Equal(t, repository.VenueKindGroupChat, kind)
}
