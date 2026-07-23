package tests

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rematchTestEnv bundles the dependencies a typical rematch test needs.
//
// Post-PR-6 cutover: `bus` is a live events.Bus with the
// InteractionRecorderWorker running in the background. Rematch/aggregation
// publishers are constructed against this bus so publish → enqueue →
// consumer write happens asynchronously (just like production). Tests
// that assert "interaction exists after rematch" call
// waitForInteractionBySourceRef to poll until the async write commits.
type rematchTestEnv struct {
	ctx               context.Context
	database          *db.Database
	gen               *factory.Generator
	contactRepo       *repository.ContactRepository
	contactMethodRepo *repository.ContactMethodRepository
	calendarRepo      *repository.CalendarEventRepository
	externalRepo      *repository.ExternalContactRepository
	interactionRepo   *repository.InteractionRepository
	contactSvc        *service.ContactService
	enrichmentSvc     *service.EnrichmentService
	rematchSvc        *service.RematchService
	bus               *events.Bus
}

func setupRematchEnv(t *testing.T) *rematchTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	// Per-test isolated clone: the live rematch dispatcher worker drains a
	// private river_job.
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

	// Build rematchSvc first so it can be passed as RematchRegistry to
	// ContactService's constructor (post-PR-10 wiring; SetRematchService
	// is gone). Handlers register AFTER the bus exists, below.
	rematchSvc := service.NewRematchService()

	cadenceUpdater := buildCadenceUpdaterForTest(t, database)
	assertSvc, cache := buildKnowledgeDeps(t, database, nil)
	contactSvc := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, rematchSvc,
		cadenceUpdater, assertSvc, cache, nil)
	enrichmentSvc := service.NewEnrichmentService(database, contactRepo, contactMethodRepo, enrichmentRepo, nil, rematchSvc,
		cadenceUpdater, assertSvc, cache)

	// Cutover wiring: live bus + InteractionRecorderWorker +
	// RematchDispatcherWorker so UpdateContact's publish flows through
	// the event bus and fires the rematch dispatcher against the
	// registered handlers. Tests wait via waitForRematchJob on the
	// in-memory job entry (RegisterPending seeds it, dispatcher Run
	// updates it).
	bus := setupTestEventBusWithRematch(t, ctx, database, contactSvc, rematchSvc)

	// Re-inject the live bus onto services now that it's constructed.
	// Services took a nil bus above because the bus depends on
	// contactSvc via the InteractionRecorder; this swap happens before
	// any test runs.
	contactSvc.InjectBusForTest(bus)
	enrichmentSvc.InjectBusForTest(bus)

	rematchSvc.Register(google.NewCalendarRematchHandler(calendarRepo, externalRepo, bus))

	gen, _ := migrationGenerator(t)

	return &rematchTestEnv{
		ctx:               ctx,
		database:          database,
		gen:               gen,
		contactRepo:       contactRepo,
		contactMethodRepo: contactMethodRepo,
		calendarRepo:      calendarRepo,
		externalRepo:      externalRepo,
		interactionRepo:   interactionRepo,
		contactSvc:        contactSvc,
		enrichmentSvc:     enrichmentSvc,
		rematchSvc:        rematchSvc,
		bus:               bus,
	}
}

// newContact seeds a namespaced, method-less contact through the lightweight
// migration helper (nil-bus single-tx write) and registers its scoped cleanup.
// Rematch is driven by an explicit attendee email/handle, so the contact needs
// no method of its own — only a namespace-isolated identity.
func (env *rematchTestEnv) newContact(t *testing.T) *repository.Contact {
	t.Helper()
	contact, cleanup := seedMigrationContact(env.ctx, t, env.database, env.gen, factory.WithNoMethods())
	t.Cleanup(cleanup)
	return contact
}

// waitForJob polls the rematch service until the job is in a terminal state.
// Bounded by maxAttempts × sleep so it doesn't hang test runs.
func waitForRematchJob(t *testing.T, svc *service.RematchService, jobID uuid.UUID) service.JobProgress {
	t.Helper()
	const maxAttempts = 400 // 400 × 5ms = 2s
	for range maxAttempts {
		j, err := svc.GetJob(jobID)
		if err == nil && j.Status != service.JobStatusRunning {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("rematch job %s did not finish in time", jobID)
	return service.JobProgress{}
}

func seedCalendarEventWithAttendee(t *testing.T, env *rematchTestEnv, accountID, email string, endTime time.Time, status string) *repository.CalendarEvent {
	t.Helper()
	title := "Test Meeting"
	resp := "accepted"
	syncedAt := accelerated.GetCurrentTime()
	event, err := env.calendarRepo.Upsert(env.ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:     "rematch-event-" + uuid.NewString(),
		GcalCalendarID:  "primary",
		GoogleAccountID: accountID,
		Title:           &title,
		StartTime:       endTime.Add(-time.Hour),
		EndTime:         endTime,
		Status:          status,
		UserResponse:    &resp,
		Attendees: []repository.Attendee{
			{Email: email, DisplayName: "Test Attendee"},
		},
		MatchedContactIDs:    []uuid.UUID{},
		SyncedAt:             syncedAt,
		LastContactedUpdated: false,
	})
	require.NoError(t, err)
	return event
}

func TestRematch_CalendarPastEvent_RecordsInteraction(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	accountID := "rematch-past-" + uuid.NewString()
	email := "alice@example.com"
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-2 * time.Hour)
	event := seedCalendarEventWithAttendee(t, env, accountID, email, pastEnd, "confirmed")

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "email", Value: email},
	})
	require.NotEqual(t, uuid.Nil, jobID)

	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 1, job.Matched)

	// matched_contact_ids should now include the contact.
	updatedEvent, err := env.calendarRepo.GetByID(env.ctx, event.ID)
	require.NoError(t, err)
	require.Contains(t, updatedEvent.MatchedContactIDs, contact.ID)

	// An interaction should exist for (contact, gcal, event.ID). Post-PR-6
	// the write is async via the event bus consumer — poll until it lands.
	eventIDStr := event.ID.String()
	interaction := waitForInteractionBySourceRef(t, env.ctx, env.interactionRepo, contact.ID, repository.InteractionSourceGCal, eventIDStr, defaultInteractionWaitTimeout)
	require.NotNil(t, interaction)
	assert.WithinDuration(t, pastEnd, interaction.OccurredAt, time.Second)

	// Contact's last_contacted should reflect the event.
	updatedContact, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, updatedContact.LastContacted)
	assert.WithinDuration(t, pastEnd, *updatedContact.LastContacted, time.Second)
}

func TestRematch_CalendarFutureEvent_NoInteraction(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	accountID := "rematch-future-" + uuid.NewString()
	email := "bob@example.com"
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	futureEnd := accelerated.GetCurrentTime().Add(48 * time.Hour)
	event := seedCalendarEventWithAttendee(t, env, accountID, email, futureEnd, "confirmed")

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "email", Value: email},
	})
	require.NotEqual(t, uuid.Nil, jobID)

	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 1, job.Matched)

	// matched_contact_ids updated.
	updatedEvent, err := env.calendarRepo.GetByID(env.ctx, event.ID)
	require.NoError(t, err)
	assert.Contains(t, updatedEvent.MatchedContactIDs, contact.ID)

	// No interaction should be recorded (future events).
	eventIDStr := event.ID.String()
	_, err = env.interactionRepo.FindBySourceRef(env.ctx, contact.ID, repository.InteractionSourceGCal, eventIDStr)
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected no interaction for future event, got %v", err)
}

func TestRematch_CalendarCaseInsensitiveEmailMatch(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	accountID := "rematch-case-" + uuid.NewString()
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	// Stored attendee email uses mixed case.
	event := seedCalendarEventWithAttendee(t, env, accountID, "Alice@Example.COM", pastEnd, "confirmed")

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "email", Value: "alice@example.com"},
	})
	require.NotEqual(t, uuid.Nil, jobID)
	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 1, job.Matched, "case-insensitive match should still find the event")

	updatedEvent, err := env.calendarRepo.GetByID(env.ctx, event.ID)
	require.NoError(t, err)
	assert.Contains(t, updatedEvent.MatchedContactIDs, contact.ID)
}

func TestRematch_CalendarIdempotent(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	accountID := "rematch-idem-" + uuid.NewString()
	email := "carol@example.com"
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	event := seedCalendarEventWithAttendee(t, env, accountID, email, pastEnd, "confirmed")

	// Run twice. The second run should be a no-op (matched=0) — query filters
	// out events that already include the contact.
	for i := range 2 {
		jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
			{Type: "email", Value: email},
		})
		require.NotEqual(t, uuid.Nil, jobID)
		job := waitForRematchJob(t, env.rematchSvc, jobID)
		assert.Equal(t, service.JobStatusCompleted, job.Status)
		if i == 0 {
			assert.Equal(t, 1, job.Matched, "first run links the event")
		} else {
			assert.Equal(t, 0, job.Matched, "second run is a no-op")
		}
	}

	// matched_contact_ids should contain the contact exactly once.
	updatedEvent, err := env.calendarRepo.GetByID(env.ctx, event.ID)
	require.NoError(t, err)
	count := 0
	for _, id := range updatedEvent.MatchedContactIDs {
		if id == contact.ID {
			count++
		}
	}
	assert.Equal(t, 1, count, "matched_contact_ids should contain the contact exactly once")

	// Exactly one interaction should exist (consumer dedupes on (source, source_ref)).
	// Poll first to let the async consumer commit the first run's write.
	eventIDStr := event.ID.String()
	_ = waitForInteractionBySourceRef(t, env.ctx, env.interactionRepo, contact.ID, repository.InteractionSourceGCal, eventIDStr, defaultInteractionWaitTimeout)
	interactions, err := env.interactionRepo.ListContactInteractions(env.ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	count = 0
	for _, it := range interactions {
		if it.Source == repository.InteractionSourceGCal && it.SourceRef != nil && *it.SourceRef == event.ID.String() {
			count++
		}
	}
	assert.Equal(t, 1, count, "exactly one interaction per (contact, source, source_ref)")
}

// The rematch append must not reset an event's last_contacted_updated flag:
// the rematch handler records interactions for past events directly, so
// flipping the flag back would make the calendar scheduler re-emit
// already-projected attendees (CAL-019's processed-flag facet — waived on
// the E2E side, owned here).
func TestRematch_AppendPreservesProcessedFlag(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	accountID := "rematch-flag-" + uuid.NewString()
	email := "flag-" + uuid.NewString()[:8] + "@example.com"
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	event := seedCalendarEventWithAttendee(t, env, accountID, email, pastEnd, "confirmed")

	// Simulate the scheduler having already projected this event's attendees.
	require.NoError(t, env.calendarRepo.MarkLastContactedUpdated(env.ctx, event.ID))

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "email", Value: email},
	})
	require.NotEqual(t, uuid.Nil, jobID)
	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 1, job.Matched)

	updatedEvent, err := env.calendarRepo.GetByID(env.ctx, event.ID)
	require.NoError(t, err)
	require.Contains(t, updatedEvent.MatchedContactIDs, contact.ID)
	assert.True(t, updatedEvent.LastContactedUpdated,
		"AppendMatchedContact must preserve last_contacted_updated so already-projected attendees are not re-emitted")
}

func TestRematch_CalendarMarksGcalAttendeeExternalContactMatched(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	email := env.gen.Prefix() + "dan@synthetic.example"
	displayName := "Dan Test"

	// Seed a gcal_attendee external_contact with this email as its source_id.
	syncedAt := accelerated.GetCurrentTime()
	external, err := env.externalRepo.Upsert(env.ctx, repository.UpsertExternalContactRequest{
		Source:      "gcal_attendee",
		SourceID:    email,
		DisplayName: &displayName,
		Emails:      []repository.EmailEntry{{Value: email, Type: "calendar"}},
		SyncedAt:    &syncedAt,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.externalRepo.Delete(env.ctx, external.ID) })

	accountID := "rematch-ext-" + uuid.NewString()
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	_ = seedCalendarEventWithAttendee(t, env, accountID, email, pastEnd, "confirmed")

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "email", Value: email},
	})
	require.NotEqual(t, uuid.Nil, jobID)
	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)

	// External candidate should now be marked matched and linked to the contact.
	updatedExternal, err := env.externalRepo.GetByID(env.ctx, external.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MatchStatusMatched, updatedExternal.MatchStatus)
	require.NotNil(t, updatedExternal.CRMContactID)
	assert.Equal(t, contact.ID, *updatedExternal.CRMContactID)
}

// TestRematch_TelegramUsernameMatch covers the username path end-to-end:
// seed unmatched messages with peer_username, run UsernameRematchHandler,
// verify messages were linked. Aggregation correctness is owned by the
// existing aggregation tests.
func TestRematch_TelegramUsernameMatch(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	// Stand up the telegram handler stack — we register the username handler
	// directly against the env's rematch service so we can assert on it.
	cfg := config.TestConfig()
	messageRepo := repository.NewTelegramMessageRepository(env.database.Queries)
	identityRepo := repository.NewIdentityRepository(env.database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	peerMatcher := tgpkg.NewPeerMatcher(identityService, messageRepo, env.externalRepo, env.enrichmentSvc, cfg.Telegram.DiscoveryMinMessages)
	aggregationEngine := tgpkg.NewAggregationEngine(
		cfg.Telegram.BurstWindowHours, cfg.Telegram.ReplyBridgeHours,
		messageRepo, env.interactionRepo,
		env.contactSvc, env.contactSvc,
		env.bus,
		nil, // pool: non-tx publish fallback (test mode)
		nil, // enqueuer: stale-claim recovery disabled
	)
	env.rematchSvc.Register(tgpkg.NewUsernameRematchHandler(messageRepo, peerMatcher, aggregationEngine))

	contact := env.newContact(t)

	suffix := uuid.NewString()[:8]
	chatID := int64(900100)
	peerUserID := int64(800100)
	username := "rematchuser_" + suffix
	t.Cleanup(func() {
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM telegram_message WHERE peer_user_id = $1", peerUserID)
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM event WHERE source = 'telegram' AND source_id LIKE $1",
			fmt.Sprintf("tg:%d:%%", chatID))
	})

	now := accelerated.GetCurrentTime()
	text := "Hello"
	for i := range 3 {
		_, err := messageRepo.UpsertMessage(env.ctx, repository.UpsertTelegramMessageParams{
			TelegramMessageID: int32(90100 + i),
			TelegramChatID:    chatID,
			ChatType:          "private",
			MessageText:       &text,
			MessageType:       "text",
			SentAt:            now.Add(-time.Duration(3-i) * time.Hour).Truncate(time.Microsecond),
			IsOutgoing:        i%2 == 0,
			PeerUserID:        ptrInt64(peerUserID),
			PeerUsername:      &username,
		})
		require.NoError(t, err)
	}

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "telegram", Value: username},
	})
	require.NotEqual(t, uuid.Nil, jobID)

	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 3, job.Matched)

	// All three messages should now be linked to the contact.
	linked, err := messageRepo.ListUnprocessedByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	// Aggregation will mark them processed; before that they're returned by
	// ListUnprocessedByContact only if processed_at IS NULL. Either way the
	// handler runs aggregation so they may now be processed. Verify via raw
	// SQL count of matched messages instead.
	_ = linked
	var matchedCount int
	require.NoError(t, env.database.Pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM telegram_message WHERE peer_user_id = $1 AND matched_contact_id = $2 AND deleted_at IS NULL",
		peerUserID, contact.ID,
	).Scan(&matchedCount))
	assert.Equal(t, 3, matchedCount, "all messages for the peer should be linked to the contact")
}

func TestRematch_NoHandlerForType_ReturnsNilJobID(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	// Only the calendar (email) handler is registered in the base env;
	// asking for rematch on a phone-only set should return uuid.Nil.
	jobID := env.rematchSvc.StartRematchForContact(uuid.New(), []service.Method{
		{Type: "phone", Value: "+15551212"},
	})
	assert.Equal(t, uuid.Nil, jobID)
}

// TestRematch_Publisher_NoHandler_ReturnsNilJobID pins the EligibleMethods
// gate on the actual event-bus publisher paths (CreateContact /
// UpdateContact / RescanRematch). Without this regression test, an
// unhandled method type would mint a jobID + enqueue a no-op river
// job. Only the email handler is registered; adding a phone method
// must produce uuid.Nil.
// spec: IMP-019[2]
func TestRematch_Publisher_NoHandler_ReturnsNilJobID(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	// CreateContact with a phone-only method — no handler registered
	// for "phone" in the base env.
	phone := "+15551212"
	_, jobID, err := env.contactSvc.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: env.gen.Prefix() + "No Handler",
	}, []service.ContactMethodInput{
		{Type: "phone", Value: phone, IsPrimary: true},
	})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, jobID, "unhandled method type must not mint a rematch jobID")

	// RescanRematch on a contact whose only methods are unhandled
	// should also return uuid.Nil.
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: env.gen.Prefix() + "Rescan No Handler",
	})
	require.NoError(t, err)
	_, err = env.contactMethodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "phone",
		Value:     phone,
		IsPrimary: true,
	})
	require.NoError(t, err)

	rescanJobID, err := env.contactSvc.RescanRematch(env.ctx, contact.ID)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, rescanJobID, "rescan with unhandled methods must return uuid.Nil")
}

// registerTelegramHandlers wires the username + phone handlers against env's
// rematch service using the same PeerMatcher and AggregationEngine the
// production manager would own. Returns the message repo for seeding.
func registerTelegramHandlers(t *testing.T, env *rematchTestEnv) *repository.TelegramMessageRepository {
	t.Helper()
	cfg := config.TestConfig()
	messageRepo := repository.NewTelegramMessageRepository(env.database.Queries)
	identityRepo := repository.NewIdentityRepository(env.database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	peerMatcher := tgpkg.NewPeerMatcher(identityService, messageRepo, env.externalRepo, env.enrichmentSvc, cfg.Telegram.DiscoveryMinMessages)
	aggregationEngine := tgpkg.NewAggregationEngine(
		cfg.Telegram.BurstWindowHours, cfg.Telegram.ReplyBridgeHours,
		messageRepo, env.interactionRepo,
		env.contactSvc, env.contactSvc,
		env.bus,
		nil, // pool: non-tx publish fallback (test mode)
		nil, // enqueuer: stale-claim recovery disabled
	)
	env.rematchSvc.Register(tgpkg.NewUsernameRematchHandler(messageRepo, peerMatcher, aggregationEngine))
	env.rematchSvc.Register(tgpkg.NewPhoneRematchHandler(messageRepo, peerMatcher, aggregationEngine))
	return messageRepo
}

func TestRematch_TelegramPhoneMatch(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)
	messageRepo := registerTelegramHandlers(t, env)

	contact := env.newContact(t)

	chatID := int64(900200)
	peerUserID := int64(800200)
	// peer_phone is raw MTProto (digits only); contact_method value_normalized
	// carries a leading '+'. Rematch must compare on digits-only.
	rawPhone := "14155550101"
	normalizedPhone := "+14155550101"
	t.Cleanup(func() {
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM telegram_message WHERE peer_user_id = $1", peerUserID)
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM event WHERE source = 'telegram' AND source_id LIKE $1",
			fmt.Sprintf("tg:%d:%%", chatID))
	})

	now := accelerated.GetCurrentTime()
	text := "Hi"
	_, err := messageRepo.UpsertMessage(env.ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90200,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            now.Add(-time.Hour).Truncate(time.Microsecond),
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(peerUserID),
		PeerPhone:         &rawPhone,
	})
	require.NoError(t, err)

	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "phone", Value: normalizedPhone},
	})
	require.NotEqual(t, uuid.Nil, jobID)
	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 1, job.Matched)

	var matchedCount int
	require.NoError(t, env.database.Pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM telegram_message WHERE peer_user_id = $1 AND matched_contact_id = $2",
		peerUserID, contact.ID,
	).Scan(&matchedCount))
	assert.Equal(t, 1, matchedCount)
}

func TestRematch_ConcurrentJobs_PerContactMutex(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	accountID := "rematch-concurrent-" + uuid.NewString()
	email1 := "conc1@example.com"
	email2 := "conc2@example.com"
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	// Seed two distinct past events so both rematch calls have work to do.
	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	seedCalendarEventWithAttendee(t, env, accountID, email1, pastEnd, "confirmed")
	seedCalendarEventWithAttendee(t, env, accountID, email2, pastEnd.Add(-30*time.Minute), "confirmed")

	// Fire two jobs targeting the same contact — the per-contact mutex should
	// serialize them, and both should ultimately complete cleanly.
	job1 := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{{Type: "email", Value: email1}})
	job2 := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{{Type: "email", Value: email2}})
	require.NotEqual(t, uuid.Nil, job1)
	require.NotEqual(t, uuid.Nil, job2)

	p1 := waitForRematchJob(t, env.rematchSvc, job1)
	p2 := waitForRematchJob(t, env.rematchSvc, job2)
	assert.Equal(t, service.JobStatusCompleted, p1.Status)
	assert.Equal(t, service.JobStatusCompleted, p2.Status)
	assert.Equal(t, 1, p1.Matched)
	assert.Equal(t, 1, p2.Matched)

	// Both events should be linked to the contact, with no torn writes.
	var linkedCount int
	require.NoError(t, env.database.Pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM calendar_event WHERE google_account_id = $1 AND $2 = ANY(matched_contact_ids)",
		accountID, contact.ID,
	).Scan(&linkedCount))
	assert.Equal(t, 2, linkedCount)
}

func TestRematch_RescanContact_RunsForAllMethods(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	contact := env.newContact(t)

	// Create the method directly on the repo so the create path doesn't
	// trigger a rematch job we don't care about.
	email := "rescan@example.com"
	_, err := env.contactMethodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      "email",
		Value:     email,
		IsPrimary: true,
	})
	require.NoError(t, err)

	accountID := "rematch-rescan-" + uuid.NewString()
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	event := seedCalendarEventWithAttendee(t, env, accountID, email, pastEnd, "confirmed")

	// Manual rescan — post-PR-10 the handler calls
	// ContactService.RescanRematch (publishes via event bus +
	// RegisterPending) instead of the deleted
	// RematchService.RescanContact.
	jobID, err := env.contactSvc.RescanRematch(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, jobID)

	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	assert.Equal(t, 1, job.Matched)

	updatedEvent, err := env.calendarRepo.GetByID(env.ctx, event.ID)
	require.NoError(t, err)
	assert.Contains(t, updatedEvent.MatchedContactIDs, contact.ID)
}

func TestRematch_RescanContact_UnknownContactReturnsNotFound(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	_, err := env.contactSvc.RescanRematch(env.ctx, uuid.New())
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound for unknown contact, got %v", err)
}

// TestRematch_TelegramRematchPlusPostImportHook_NoDuplicateInteraction
// simulates what happens on the Telegram import/link HTTP path: rematch
// links the messages and runs aggregation, AND the PostImportHook (which
// ImportContact / LinkContact still call) runs afterwards and performs the
// same work. The DB unique index on (contact_id, source, source_ref) must
// prevent duplicate interactions even though the two paths overlap.
// Regression test for plan Test Case 5.
func TestRematch_TelegramRematchPlusPostImportHook_NoDuplicateInteraction(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	// Build the shared matcher + aggregator — the manager normally owns these.
	cfg := config.TestConfig()
	messageRepo := repository.NewTelegramMessageRepository(env.database.Queries)
	identityRepo := repository.NewIdentityRepository(env.database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	peerMatcher := tgpkg.NewPeerMatcher(identityService, messageRepo, env.externalRepo, env.enrichmentSvc, cfg.Telegram.DiscoveryMinMessages)
	aggregationEngine := tgpkg.NewAggregationEngine(
		cfg.Telegram.BurstWindowHours, cfg.Telegram.ReplyBridgeHours,
		messageRepo, env.interactionRepo,
		env.contactSvc, env.contactSvc,
		env.bus,
		nil, // pool: non-tx publish fallback (test mode)
		nil, // enqueuer: stale-claim recovery disabled
	)
	env.rematchSvc.Register(tgpkg.NewUsernameRematchHandler(messageRepo, peerMatcher, aggregationEngine))

	contact := env.newContact(t)

	suffix := uuid.NewString()[:8]
	chatID := int64(900300)
	peerUserID := int64(800300)
	username := "combinedpath_" + suffix
	t.Cleanup(func() {
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM telegram_message WHERE peer_user_id = $1", peerUserID)
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM interaction WHERE contact_id = $1 AND source = 'telegram'", contact.ID)
		// Also clean up published events — chatID is fixed so a re-run
		// collides on (source, source_id) dedup and the consumer never
		// fires.
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM event WHERE source = 'telegram' AND source_id LIKE $1",
			fmt.Sprintf("tg:%d:%%", chatID))
	})

	// Seed three outbound messages so the aggregation engine produces a single
	// outbound-burst interaction.
	now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "ping"
	for i := range 3 {
		_, err := messageRepo.UpsertMessage(env.ctx, repository.UpsertTelegramMessageParams{
			TelegramMessageID: int32(90300 + i),
			TelegramChatID:    chatID,
			ChatType:          "private",
			MessageText:       &text,
			MessageType:       "text",
			SentAt:            now.Add(-time.Duration(3-i) * 30 * time.Minute),
			IsOutgoing:        true,
			PeerUserID:        ptrInt64(peerUserID),
			PeerUsername:      &username,
		})
		require.NoError(t, err)
	}

	// Step 1: rematch — this mirrors what UpdateContact dispatches when the
	// user adds a telegram handle to an existing contact.
	jobID := env.rematchSvc.StartRematchForContact(contact.ID, []service.Method{
		{Type: "telegram", Value: username},
	})
	require.NotEqual(t, uuid.Nil, jobID)
	job := waitForRematchJob(t, env.rematchSvc, jobID)
	require.Equal(t, service.JobStatusCompleted, job.Status)
	require.Equal(t, 3, job.Matched)

	// Step 2: simulate the ImportContact/LinkContact PostImportHook firing
	// AFTER rematch. peerMatcher.OnPeerLinked is idempotent
	// (matched_contact_id IS NULL filter) and aggregation reruns over the
	// already-processed messages (processed_at IS NULL filter) — both should
	// be no-ops.
	require.NoError(t, peerMatcher.OnPeerLinked(env.ctx, peerUserID, username, contact.ID))
	require.NoError(t, aggregationEngine.AggregateForContactBatch(env.ctx, contact.ID))

	// Invariant: exactly one telegram interaction for this contact. Post-PR-6
	// the write is async — poll for it to appear, then assert uniqueness.
	_ = waitForTelegramInteractionCount(t, env.ctx, env.database.Pool, contact.ID, 1, defaultInteractionWaitTimeout)
	var total int
	require.NoError(t, env.database.Pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM interaction WHERE contact_id = $1 AND source = 'telegram' AND deleted_at IS NULL",
		contact.ID,
	).Scan(&total))
	assert.Equal(t, 1, total, "rematch + PostImportHook together must not produce duplicate interactions")

	// All seeded messages should be linked and processed. The consumer marks
	// messages processed inside the interaction-insert tx (plan Decision 10);
	// wait for all 3 before asserting.
	waitForTelegramMessagesProcessed(t, env.ctx, env.database.Pool, peerUserID, contact.ID, 3, defaultInteractionWaitTimeout)
}

// Rematch is dispatched for method values that are newly PRESENT, not for
// every method named in the request. The trigger moved from the contact PUT to
// the operations endpoint when the PUT's method-replacing branch was retired;
// the diff behavior it asserts is unchanged.
// spec: IMP-019[0]
func TestRematch_ApplyMethodOperations_FiresForNewMethodOnly(t *testing.T) {
	t.Parallel()
	env := setupRematchEnv(t)

	existingEmail := "existing@example.com"
	contact := env.newContact(t)

	// Seed an existing email method outside the service so no rematch fires.
	_, err := env.contactMethodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID, Type: "email", Value: existingEmail, IsPrimary: true,
	})
	require.NoError(t, err)

	accountID := "rematch-diff-" + uuid.NewString()
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	// Event whose attendee matches the PRE-EXISTING email. Rematch should
	// NOT link this — the semantic diff filters an already-present value out.
	_ = seedCalendarEventWithAttendee(t, env, accountID, existingEmail, pastEnd, "confirmed")
	// Event whose attendee matches the NEWLY-ADDED email. Should be linked.
	newEmail := "added@example.com"
	newEvent := seedCalendarEventWithAttendee(t, env, accountID, newEmail, pastEnd, "confirmed")

	methodSvc := service.NewContactMethodService(env.database, env.bus, env.rematchSvc)
	result, err := methodSvc.ApplyOperations(env.ctx, contact.ID, []service.ContactMethodOperation{
		// Already present: resolves to the existing row, so it is absent from
		// the semantic diff and must not contribute a rematch.
		{Op: service.MethodOpAdd, Type: "email", Value: existingEmail},
		{Op: service.MethodOpAdd, Type: "email", Value: newEmail},
	})
	require.NoError(t, err)
	jobID := result.RematchJobID
	require.NotEqual(t, uuid.Nil, jobID, "ApplyOperations should dispatch rematch for the newly-added email")

	job := waitForRematchJob(t, env.rematchSvc, jobID)
	assert.Equal(t, service.JobStatusCompleted, job.Status)
	// matched is the number of new events linked — only the newEmail event.
	assert.Equal(t, 1, job.Matched)

	// Verify: new event linked, existing-email event still unlinked from this
	// contact (since rematch was only for the new email diff).
	newEventState, err := env.calendarRepo.GetByID(env.ctx, newEvent.ID)
	require.NoError(t, err)
	assert.Contains(t, newEventState.MatchedContactIDs, contact.ID)
}
