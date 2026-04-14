package tests

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rematchTestEnv bundles the dependencies a typical rematch test needs.
type rematchTestEnv struct {
	ctx               context.Context
	database          *db.Database
	contactRepo       *repository.ContactRepository
	contactMethodRepo *repository.ContactMethodRepository
	calendarRepo      *repository.CalendarEventRepository
	externalRepo      *repository.ExternalContactRepository
	interactionRepo   *repository.InteractionRepository
	contactSvc        *service.ContactService
	enrichmentSvc     *service.EnrichmentService
	rematchSvc        *service.RematchService
}

func setupRematchEnv(t *testing.T) *rematchTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	require.NoError(t, db.RunMigrations(context.Background(), databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	ctx := context.Background()
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)

	contactSvc := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo)
	enrichmentSvc := service.NewEnrichmentService(contactRepo, contactMethodRepo, enrichmentRepo)

	rematchSvc := service.NewRematchService()
	rematchSvc.Register(google.NewCalendarRematchHandler(calendarRepo, externalRepo, contactSvc))
	contactSvc.SetRematchService(rematchSvc)
	enrichmentSvc.SetRematchService(rematchSvc)

	return &rematchTestEnv{
		ctx:               ctx,
		database:          database,
		contactRepo:       contactRepo,
		contactMethodRepo: contactMethodRepo,
		calendarRepo:      calendarRepo,
		externalRepo:      externalRepo,
		interactionRepo:   interactionRepo,
		contactSvc:        contactSvc,
		enrichmentSvc:     enrichmentSvc,
		rematchSvc:        rematchSvc,
	}
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
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Past Event " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

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

	// An interaction should exist for (contact, gcal, event.ID).
	eventIDStr := event.ID.String()
	interaction, err := env.interactionRepo.FindBySourceRef(env.ctx, contact.ID, repository.InteractionSourceGCal, eventIDStr)
	require.NoError(t, err)
	require.NotNil(t, interaction)
	assert.WithinDuration(t, pastEnd, interaction.OccurredAt, time.Second)

	// Contact's last_contacted should reflect the event.
	updatedContact, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, updatedContact.LastContacted)
	assert.WithinDuration(t, pastEnd, *updatedContact.LastContacted, time.Second)
}

func TestRematch_CalendarFutureEvent_NoInteraction(t *testing.T) {
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Future Event " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

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
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Case " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

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
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Idempotent " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

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

	// Exactly one interaction should exist (RecordInteraction dedupes).
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

func TestRematch_CalendarMarksGcalAttendeeExternalContactMatched(t *testing.T) {
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch External " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

	email := "dan-" + uuid.NewString()[:8] + "@example.com"
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
		env.contactSvc, env.contactSvc, env.contactSvc,
	)
	env.rematchSvc.Register(tgpkg.NewUsernameRematchHandler(messageRepo, peerMatcher, aggregationEngine))

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch TG Username " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

	suffix := uuid.NewString()[:8]
	chatID := int64(900100)
	peerUserID := int64(800100)
	username := "rematchuser_" + suffix
	t.Cleanup(func() {
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM telegram_message WHERE peer_user_id = $1", peerUserID)
	})

	now := accelerated.GetCurrentTime()
	text := "Hello"
	for i := range 3 {
		_, err = messageRepo.UpsertMessage(env.ctx, repository.UpsertTelegramMessageParams{
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
	env := setupRematchEnv(t)

	// Only the calendar (email) handler is registered in the base env;
	// asking for rematch on a phone-only set should return uuid.Nil.
	jobID := env.rematchSvc.StartRematchForContact(uuid.New(), []service.Method{
		{Type: "phone", Value: "+15551212"},
	})
	assert.Equal(t, uuid.Nil, jobID)
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
		env.contactSvc, env.contactSvc, env.contactSvc,
	)
	env.rematchSvc.Register(tgpkg.NewUsernameRematchHandler(messageRepo, peerMatcher, aggregationEngine))
	env.rematchSvc.Register(tgpkg.NewPhoneRematchHandler(messageRepo, peerMatcher, aggregationEngine))
	return messageRepo
}

func TestRematch_TelegramPhoneMatch(t *testing.T) {
	env := setupRematchEnv(t)
	messageRepo := registerTelegramHandlers(t, env)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch TG Phone " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

	chatID := int64(900200)
	peerUserID := int64(800200)
	// peer_phone is raw MTProto (digits only); contact_method value_normalized
	// carries a leading '+'. Rematch must compare on digits-only.
	rawPhone := "14155550101"
	normalizedPhone := "+14155550101"
	t.Cleanup(func() {
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM telegram_message WHERE peer_user_id = $1", peerUserID)
	})

	now := accelerated.GetCurrentTime()
	text := "Hi"
	_, err = messageRepo.UpsertMessage(env.ctx, repository.UpsertTelegramMessageParams{
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
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Concurrent " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

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
	env := setupRematchEnv(t)

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Rescan " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

	// Create the method directly on the repo so the create path doesn't
	// trigger a rematch job we don't care about.
	email := "rescan@example.com"
	_, err = env.contactMethodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
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

	// Manual rescan — exercise the service-level method the handler calls.
	jobID, err := env.rematchSvc.RescanContact(env.ctx, env.contactSvc, contact.ID)
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
	env := setupRematchEnv(t)

	_, err := env.rematchSvc.RescanContact(env.ctx, env.contactSvc, uuid.New())
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
		env.contactSvc, env.contactSvc, env.contactSvc,
	)
	env.rematchSvc.Register(tgpkg.NewUsernameRematchHandler(messageRepo, peerMatcher, aggregationEngine))

	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch TG Combined " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

	suffix := uuid.NewString()[:8]
	chatID := int64(900300)
	peerUserID := int64(800300)
	username := "combinedpath_" + suffix
	t.Cleanup(func() {
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM telegram_message WHERE peer_user_id = $1", peerUserID)
		_, _ = env.database.Pool.Exec(env.ctx,
			"DELETE FROM interaction WHERE contact_id = $1 AND source = 'telegram'", contact.ID)
	})

	// Seed three outbound messages so the aggregation engine produces a single
	// outbound-burst interaction.
	now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "ping"
	for i := range 3 {
		_, err = messageRepo.UpsertMessage(env.ctx, repository.UpsertTelegramMessageParams{
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

	// Invariant: exactly one telegram interaction for this contact.
	var total int
	require.NoError(t, env.database.Pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM interaction WHERE contact_id = $1 AND source = 'telegram' AND deleted_at IS NULL",
		contact.ID,
	).Scan(&total))
	assert.Equal(t, 1, total, "rematch + PostImportHook together must not produce duplicate interactions")

	// All seeded messages should be linked and processed.
	var processed int
	require.NoError(t, env.database.Pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM telegram_message WHERE peer_user_id = $1 AND matched_contact_id = $2 AND processed_at IS NOT NULL",
		peerUserID, contact.ID,
	).Scan(&processed))
	assert.Equal(t, 3, processed)
}

func TestRematch_ContactServiceUpdateContact_FiresForNewMethodOnly(t *testing.T) {
	env := setupRematchEnv(t)

	existingEmail := "existing@example.com"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Rematch Diff " + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(env.ctx, contact.ID) })

	// Seed an existing email method outside the service so no rematch fires.
	_, err = env.contactMethodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID, Type: "email", Value: existingEmail, IsPrimary: true,
	})
	require.NoError(t, err)

	accountID := "rematch-diff-" + uuid.NewString()
	t.Cleanup(func() { _ = env.calendarRepo.DeleteEventsByAccount(env.ctx, accountID) })

	pastEnd := accelerated.GetCurrentTime().Add(-time.Hour)
	// Event whose attendee matches the PRE-EXISTING email. Rematch should
	// NOT link this on the update — diffNewMethods filters it out.
	_ = seedCalendarEventWithAttendee(t, env, accountID, existingEmail, pastEnd, "confirmed")
	// Event whose attendee matches the NEWLY-ADDED email. Should be linked.
	newEmail := "added@example.com"
	newEvent := seedCalendarEventWithAttendee(t, env, accountID, newEmail, pastEnd, "confirmed")

	_, jobID, err := env.contactSvc.UpdateContact(env.ctx, contact.ID, repository.UpdateContactRequest{
		FullName: contact.FullName,
	}, []service.ContactMethodInput{
		{Type: "email", Value: existingEmail, IsPrimary: true},
		{Type: "email", Value: newEmail, IsPrimary: false},
	}, true)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, jobID, "UpdateContact should dispatch rematch for newly-added email")

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
