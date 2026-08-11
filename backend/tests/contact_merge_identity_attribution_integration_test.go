//go:build integration_testdb

package tests

import (
	"bytes"
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeCloseNoopWorker satisfies the todoist_followup_close kind on the
// harness's insert-only river client so MergeContacts' close-job enqueues
// validate at insert time. Never runs (the client is not started).
type mergeCloseNoopWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCloseJobArgs]
}

func (*mergeCloseNoopWorker) Work(context.Context, *river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) error {
	return nil
}

// mergeAttrHarness bundles a fully-wired ContactService plus the identity
// service and the staging/task repositories a merge-attribution test reads
// back. It uses a per-test isolated clone (newIsolatedRiverTestDB) so the
// insert-only River client used for the contact_task close-job enqueue path
// does not steal jobs from parallel tests.
type mergeAttrHarness struct {
	database     *db.Database
	cfg          *config.Config
	contactSvc   *service.ContactService
	identitySvc  *service.IdentityService
	contactRepo  *repository.ContactRepository
	methodRepo   *repository.ContactMethodRepository
	identityRepo *repository.IdentityRepository
	externalRepo *repository.ExternalContactRepository
	messagesRepo *repository.MessagesMessageRepository
	telegramRepo *repository.TelegramMessageRepository
	phoneRepo    *repository.PhoneCallRepository
	commsRepo    *repository.CommsMessageRepository
	taskRepo     *repository.ContactTaskRepository
}

// newMergeAttrHarness builds the default harness: the task-close enqueuer is
// wired (like production main.go in cutover mode). Tests exercising the D9
// wiring contract use newMergeAttrHarnessWith directly.
func newMergeAttrHarness(t *testing.T, ctx context.Context) *mergeAttrHarness {
	t.Helper()
	return newMergeAttrHarnessWith(t, ctx, true, true)
}

func newMergeAttrHarnessWith(t *testing.T, ctx context.Context, wireTaskCloseEnqueuer, remoteCloseEnabled bool) *mergeAttrHarness {
	t.Helper()
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)
	telegramRepo := repository.NewTelegramMessageRepository(database.Queries)
	phoneRepo := repository.NewPhoneCallRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)

	// Insert-only River client (no fetch loop): AssertService.PublishTx
	// enqueues assertion.* jobs the no-op worker registers; the knowledge
	// cache is filled inline. The same client backs the contact_task
	// close-job enqueue path exercised by the merge tests.
	workers := river.NewWorkers()
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	river.AddWorker(workers, &mergeCloseNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, eventRepo)

	assertSvc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)
	cacheUpdater := consumer.NewKnowledgeCacheUpdater(assertionRepo, nodeRepo, contactRepo)

	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil,
		buildCadenceUpdaterForTest(t, database), assertSvc, cacheUpdater, nil)
	if wireTaskCloseEnqueuer {
		contactSvc.SetTaskCloseEnqueuer(client, remoteCloseEnabled)
	}

	identitySvc := service.NewIdentityService(identityRepo)

	return &mergeAttrHarness{
		database:     database,
		cfg:          cfg,
		contactSvc:   contactSvc,
		identitySvc:  identitySvc,
		contactRepo:  contactRepo,
		methodRepo:   methodRepo,
		identityRepo: identityRepo,
		externalRepo: externalRepo,
		messagesRepo: messagesRepo,
		telegramRepo: telegramRepo,
		phoneRepo:    phoneRepo,
		commsRepo:    commsRepo,
		taskRepo:     taskRepo,
	}
}

// createContact creates a bare contact via the wired ContactService.
func (h *mergeAttrHarness) createContact(t *testing.T, ctx context.Context, name string) *repository.Contact {
	t.Helper()
	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{FullName: name}, nil)
	require.NoError(t, err)
	return contact
}

// addPhone attaches a phone contact_method so the identity discovery path can
// find the contact by its normalized value.
func (h *mergeAttrHarness) addPhone(t *testing.T, ctx context.Context, contactID uuid.UUID, phone string) {
	t.Helper()
	_, err := h.methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      "phone",
		Value:     phone,
	})
	require.NoError(t, err)
}

// TestMergeContacts_IdentityAttribution_FollowsSurvivor is the core RED→GREEN
// reproduction: once a message from B's handle has been ingested (caching
// external_identity.contact_id = B), a merge of B into A must re-point the
// identity cache so a subsequent match on the same handle attributes to the
// survivor A — not the tombstoned loser B.
func TestMergeContacts_IdentityAttribution_FollowsSurvivor(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)

	const phone = "+15558675309"

	// A is the survivor (winner); B is the loser (source).
	a := h.createContact(t, ctx, "merge-attr survivor")
	b := h.createContact(t, ctx, "merge-attr loser")
	h.addPhone(t, ctx, b.ID, phone)

	// Seed the identity cache the realistic way: discovery finds B (B owns
	// the phone) and caches external_identity(phone, messages).contact_id = B.
	pre, err := h.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier: phone,
		Type:          identity.IdentifierTypePhone,
		Source:        "messages",
	})
	require.NoError(t, err)
	require.NotNil(t, pre.ContactID)
	require.Equal(t, b.ID, *pre.ContactID, "pre-merge attribution should point at B")

	// Merge B into A. The transfer step moves B's phone method to A.
	_, err = h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: b.ID,
		TargetContactID: a.ID,
	})
	require.NoError(t, err)

	// A subsequent tx-bound match on the SAME handle must attribute to the
	// survivor A. Today this returns the tombstoned B (the cache short-circuits
	// before the liveness-aware discovery path).
	tx, err := h.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	post, err := h.identitySvc.MatchOrCreateTx(ctx, tx, service.MatchRequest{
		RawIdentifier: phone,
		Type:          identity.IdentifierTypePhone,
		Source:        "messages",
	}, service.NormalizationFailEmpty)
	require.NoError(t, err)
	require.NotNil(t, post.ContactID)
	assert.Equal(t, a.ID, *post.ContactID, "post-merge attribution must follow the survivor A")
}

// TestMergeContacts_RepointsIdentityAndAttribution asserts the historical-row
// half of the fix: after merging B into A, the identity-cache row, the
// external_contact import link, and a pre-merge B-attributed row in EACH of
// the four ingest-staging tables all point at A.
func TestMergeContacts_RepointsIdentityAndAttribution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)
	now := accelerated.GetCurrentTime()

	const phone = "+15550100001"
	a := h.createContact(t, ctx, "repoint survivor")
	b := h.createContact(t, ctx, "repoint loser")
	h.addPhone(t, ctx, b.ID, phone)

	// Identity cache → B.
	pre, err := h.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
		RawIdentifier: phone,
		Type:          identity.IdentifierTypePhone,
		Source:        "messages",
	})
	require.NoError(t, err)
	require.NotNil(t, pre.ContactID)
	require.Equal(t, b.ID, *pre.ContactID)

	// external_contact linked to B.
	ec, err := h.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "google_contacts",
		SourceID: "merge-attr-ec-1",
	})
	require.NoError(t, err)
	_, err = h.externalRepo.UpdateMatch(ctx, ec.ID, &b.ID, repository.MatchStatusMatched)
	require.NoError(t, err)

	// One B-attributed staging row per table.
	_, err = h.messagesRepo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
		Guid:             "merge-attr-guid-1",
		ChatGuid:         "merge-attr-chat-1",
		PeerHandle:       phone,
		MessageType:      "text",
		SentAt:           now,
		MatchedContactID: &b.ID,
	})
	require.NoError(t, err)

	peerUserID := int64(910001)
	_, err = h.telegramRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 42001,
		TelegramChatID:    910001,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            now,
		PeerUserID:        &peerUserID,
	})
	require.NoError(t, err)
	require.NoError(t, h.telegramRepo.UpdateMessageContact(ctx, peerUserID, b.ID))

	_, err = h.phoneRepo.UpsertCall(ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:     "merge-attr-call-1",
		PeerHandle:       phone,
		PeerNormalized:   identity.Normalize(phone, identity.IdentifierTypePhone),
		Service:          "voice",
		Direction:        "inbound",
		DurationSeconds:  30,
		StartedAt:        now,
		MatchedContactID: &b.ID,
	})
	require.NoError(t, err)

	gmailID := "gm-merge-attr-1"
	_, err = h.commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
		Source:           "email",
		ExternalID:       "merge-attr-email-1",
		Direction:        "inbound",
		SentAt:           now,
		SourceMetadata:   []byte(`{}`),
		MatchedContactID: b.ID,
		GmailMessageID:   &gmailID,
	})
	require.NoError(t, err)

	// Merge B → A.
	_, err = h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: b.ID,
		TargetContactID: a.ID,
	})
	require.NoError(t, err)

	// Identity cache repointed.
	ident, err := h.identityRepo.GetByIdentifier(ctx, identity.IdentifierTypePhone,
		identity.Normalize(phone, identity.IdentifierTypePhone), "messages")
	require.NoError(t, err)
	require.NotNil(t, ident.ContactID)
	assert.Equal(t, a.ID, *ident.ContactID, "external_identity repointed to A")

	// external_contact repointed.
	ecAfter, err := h.externalRepo.GetByID(ctx, ec.ID)
	require.NoError(t, err)
	require.NotNil(t, ecAfter.CRMContactID)
	assert.Equal(t, a.ID, *ecAfter.CRMContactID, "external_contact.crm_contact_id repointed to A")

	// All four staging tables repointed.
	mm, err := h.messagesRepo.GetMessage(ctx, "merge-attr-guid-1")
	require.NoError(t, err)
	require.NotNil(t, mm.MatchedContactID)
	assert.Equal(t, a.ID, *mm.MatchedContactID, "messages_message repointed to A")

	tm, err := h.telegramRepo.GetMessage(ctx, 910001, 42001)
	require.NoError(t, err)
	require.NotNil(t, tm.MatchedContactID)
	assert.Equal(t, a.ID, *tm.MatchedContactID, "telegram_message repointed to A")

	pc, err := h.phoneRepo.GetCallByUniqueID(ctx, "merge-attr-call-1")
	require.NoError(t, err)
	require.NotNil(t, pc.MatchedContactID)
	assert.Equal(t, a.ID, *pc.MatchedContactID, "phone_call repointed to A")

	cm, err := h.commsRepo.GetMessage(ctx, "email", "merge-attr-email-1", a.ID)
	require.NoError(t, err)
	require.NotNil(t, cm.MatchedContactID)
	assert.Equal(t, a.ID, *cm.MatchedContactID, "comms_message repointed to A")
}

// TestMergeContacts_CommsMessageDedupCollision covers the email-fanout shape:
// one upstream message stored live under BOTH contacts. A bare repoint would
// abort the merge with a 23505 on idx_comms_message_dedup; the two-step
// (soft-delete B's colliding copy, then repoint the rest) must succeed.
func TestMergeContacts_CommsMessageDedupCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)
	now := accelerated.GetCurrentTime()

	a := h.createContact(t, ctx, "comms dedup survivor")
	b := h.createContact(t, ctx, "comms dedup loser")

	gmailID := "gm-fanout-1"
	seed := func(extID string, contactID uuid.UUID) {
		t.Helper()
		_, err := h.commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source:           "email",
			ExternalID:       extID,
			Direction:        "inbound",
			SentAt:           now,
			SourceMetadata:   []byte(`{}`),
			MatchedContactID: contactID,
			GmailMessageID:   &gmailID,
		})
		require.NoError(t, err)
	}
	// The fanout collision: same (source, external_id) live under A AND B.
	seed("fanout-1", a.ID)
	seed("fanout-1", b.ID)
	// A non-colliding B row that must simply follow the survivor.
	seed("solo-1", b.ID)

	_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: b.ID,
		TargetContactID: a.ID,
	})
	require.NoError(t, err, "merge must not abort on the comms dedup collision")

	// A's fanout row is untouched and is the ONLY live row for that message.
	fanout, err := h.commsRepo.GetMessage(ctx, "email", "fanout-1", a.ID)
	require.NoError(t, err)
	require.NotNil(t, fanout.MatchedContactID)
	assert.Equal(t, a.ID, *fanout.MatchedContactID)

	// B's non-colliding row followed the survivor.
	solo, err := h.commsRepo.GetMessage(ctx, "email", "solo-1", a.ID)
	require.NoError(t, err)
	require.NotNil(t, solo.MatchedContactID)
	assert.Equal(t, a.ID, *solo.MatchedContactID)

	// No live rows remain attributed to B (the colliding copy was
	// soft-deleted, then repointed as a tombstone).
	remaining, err := h.commsRepo.ListByContact(ctx, b.ID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "no live comms rows may remain on the tombstoned B")

	// Exactly one live row for the fanned-out message overall.
	aRows, err := h.commsRepo.ListByContact(ctx, a.ID)
	require.NoError(t, err)
	fanoutCount := 0
	for _, row := range aRows {
		if row.ExternalID == "fanout-1" {
			fanoutCount++
		}
	}
	assert.Equal(t, 1, fanoutCount, "exactly one live copy of the fanned-out message")
}

// TestCommsRepointForMerge_RacedInsertRetries deterministically exercises the
// savepoint-retry path in RepointContactForMergeTx: a second connection
// commits A's copy of the message between the dedup and repoint statements
// (via the test barrier), the repoint hits 23505 on idx_comms_message_dedup,
// and the scoped retry must recover WITHOUT aborting the outer tx. This test
// fails if the savepoint/retry is removed.
func TestCommsRepointForMerge_RacedInsertRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)
	now := accelerated.GetCurrentTime()

	a := h.createContact(t, ctx, "comms race survivor")
	b := h.createContact(t, ctx, "comms race loser")

	gmailID := "gm-race-1"
	// B's live row exists BEFORE the repoint; A has none yet, so the first
	// attempt's dedup statement sees nothing to tombstone.
	_, err := h.commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
		Source:           "email",
		ExternalID:       "race-1",
		Direction:        "inbound",
		SentAt:           now,
		SourceMetadata:   []byte(`{}`),
		MatchedContactID: b.ID,
		GmailMessageID:   &gmailID,
	})
	require.NoError(t, err)

	// Barrier: commit A's copy from a SECOND pool connection between the
	// dedup and repoint statements of the first savepoint attempt.
	h.commsRepo.SetRepointBarrierForTest(func() {
		_, berr := h.commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source:           "email",
			ExternalID:       "race-1",
			Direction:        "inbound",
			SentAt:           now,
			SourceMetadata:   []byte(`{}`),
			MatchedContactID: a.ID,
			GmailMessageID:   &gmailID,
		})
		require.NoError(t, berr)
	})

	tx, err := h.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	require.NoError(t, h.commsRepo.RepointContactForMergeTx(ctx, tx, b.ID, a.ID),
		"savepoint retry must recover from the raced insert without aborting the outer tx")

	// The outer tx is still usable (NOT aborted) — commit must succeed.
	require.NoError(t, tx.Commit(ctx))

	// B's raced-colliding row was tombstoned by the retry's dedup pass; A's
	// row survives as the single live copy.
	live, err := h.commsRepo.GetMessage(ctx, "email", "race-1", a.ID)
	require.NoError(t, err)
	require.NotNil(t, live.MatchedContactID)
	assert.Equal(t, a.ID, *live.MatchedContactID)
	remaining, err := h.commsRepo.ListByContact(ctx, b.ID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "no live comms rows may remain on B after the retried repoint")
}

// TestMergeContacts_SecondPassRepointsRacedRows deterministically exercises
// the D8 post-commit second pass: rows committed by a concurrent ingest AFTER
// the in-tx repoints took their snapshots (simulated via the merge commit
// barrier) must still end up attributed to the survivor. This test fails if
// the post-commit second pass is removed.
func TestMergeContacts_SecondPassRepointsRacedRows(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)
	now := accelerated.GetCurrentTime()

	a := h.createContact(t, ctx, "second-pass survivor")
	b := h.createContact(t, ctx, "second-pass loser")

	var racedExternalContactID uuid.UUID
	h.contactSvc.InjectMergeCommitBarrierForTest(func() {
		// Second-connection commits while the merge tx is still open: a
		// B-attributed staging row AND a B-linked external_contact. Both
		// land AFTER the in-tx repoint statements ran, so only the
		// post-commit second pass can fix them.
		_, err := h.messagesRepo.UpsertMessage(ctx, repository.UpsertMessagesMessageParams{
			Guid:             "raced-guid-1",
			ChatGuid:         "raced-chat-1",
			PeerHandle:       "+15550100002",
			MessageType:      "text",
			SentAt:           now,
			MatchedContactID: &b.ID,
		})
		require.NoError(t, err)

		ec, err := h.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
			Source:   "google_contacts",
			SourceID: "raced-ec-1",
		})
		require.NoError(t, err)
		_, err = h.externalRepo.UpdateMatch(ctx, ec.ID, &b.ID, repository.MatchStatusMatched)
		require.NoError(t, err)
		racedExternalContactID = ec.ID
	})

	_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: b.ID,
		TargetContactID: a.ID,
	})
	require.NoError(t, err)

	mm, err := h.messagesRepo.GetMessage(ctx, "raced-guid-1")
	require.NoError(t, err)
	require.NotNil(t, mm.MatchedContactID)
	assert.Equal(t, a.ID, *mm.MatchedContactID, "second pass repointed the raced staging row")

	ec, err := h.externalRepo.GetByID(ctx, racedExternalContactID)
	require.NoError(t, err)
	require.NotNil(t, ec.CRMContactID)
	assert.Equal(t, a.ID, *ec.CRMContactID, "second pass repointed the raced external_contact link")
}

// seedTask creates a contact_task row directly through the repository with an
// explicit (kind, lifecycle, state, external id, metadata) shape.
func (h *mergeAttrHarness) seedTask(t *testing.T, ctx context.Context, contactID uuid.UUID, lifecycle, state, externalID string, metadata map[string]any) *repository.ContactTask {
	t.Helper()
	task, err := h.taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       todoist.SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      lifecycle,
		ExternalTaskID: externalID,
		State:          state,
		Metadata:       metadata,
	})
	require.NoError(t, err)
	return task
}

// requireTaskState re-reads a task and asserts its state and contact.
func (h *mergeAttrHarness) requireTaskState(t *testing.T, ctx context.Context, id uuid.UUID, state repository.ContactTaskState, contactID uuid.UUID) {
	t.Helper()
	task, err := h.taskRepo.GetContactTask(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, state, task.State)
	assert.Equal(t, contactID, task.ContactID)
}

// closeJobCount counts todoist_followup_close river jobs for a task.
func (h *mergeAttrHarness) closeJobCount(t *testing.T, ctx context.Context, taskID uuid.UUID) int64 {
	t.Helper()
	n, err := h.taskRepo.CountRiverJobsByContactTask(ctx, "todoist_followup_close", taskID)
	require.NoError(t, err)
	return n
}

// seedTaskWithIdempotencyKey seeds a contact_task carrying an idempotency_key,
// which the plain CreateContactTask path (seedTask) cannot set. Used only by
// the transfer idempotency-guard subtest.
func (h *mergeAttrHarness) seedTaskWithIdempotencyKey(t *testing.T, ctx context.Context, contactID uuid.UUID, lifecycle, state, externalID, idempotencyKey string) *repository.ContactTask {
	t.Helper()
	tx, err := h.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := h.taskRepo.CreateContactTaskTx(ctx, tx, repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       todoist.SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      lifecycle,
		ExternalTaskID: externalID,
		State:          state,
	}, &idempotencyKey)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	return task
}

// TestMergeContacts_ClosesLiveContactTasks covers the D4 lifecycle split:
// automated rows (cadence_due / followup_loop) close (with a durable remote
// close job only for real external ids), manual rows repoint to the survivor.
func TestMergeContacts_ClosesLiveContactTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)

	t.Run("real external id closes and enqueues, colliding target row survives", func(t *testing.T) {
		a := h.createContact(t, ctx, "task close survivor 1")
		b := h.createContact(t, ctx, "task close loser 1")
		// COLLISION SHAPE: A and B EACH have a live follow-up. A repoint
		// would violate idx_contact_task_followup_unique_live; the close
		// path must not.
		aTask := h.seedTask(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-real-a1", nil)
		bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-real-b1", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err, "close-not-repoint must avoid the unique-index collision")

		h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateCompleted, b.ID)
		h.requireTaskState(t, ctx, aTask.ID, repository.ContactTaskStateManaged, a.ID)
		assert.EqualValues(t, 1, h.closeJobCount(t, ctx, bTask.ID), "close job enqueued for B's real-id follow-up")
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, aTask.ID), "no close job for the survivor's own follow-up")
	})

	t.Run("pending shapes close locally without a close job", func(t *testing.T) {
		a := h.createContact(t, ctx, "task close survivor 2")
		b := h.createContact(t, ctx, "task close loser 2")
		// A holds a live row in EACH automated lifecycle so the transfer
		// guard blocks both of B's rows below and they fall back to the
		// close path — this subtest is about the close path's no-close-job
		// guarantee for pending/temp shapes, not the transfer guard itself
		// (which the transfer subtests below cover on their own).
		h.seedTask(t, ctx, a.ID, contacttask.LifecycleCadenceDue, "managed", "tsk-blocker-cad-a2", nil)
		h.seedTask(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-blocker-fu-a2", nil)
		// pending_remote_create follow-up: no external id yet.
		pendingTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "pending_remote_create", "", nil)
		// cadence row still carrying its Todoist temp id.
		tempID := "temp-cadence-b2"
		tempTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleCadenceDue, "managed", tempID,
			map[string]any{todoist.MetadataKeyPendingTempID: tempID})

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		h.requireTaskState(t, ctx, pendingTask.ID, repository.ContactTaskStateCompleted, b.ID)
		h.requireTaskState(t, ctx, tempTask.ID, repository.ContactTaskStateCompleted, b.ID)
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, pendingTask.ID), "no close job for an empty external id")
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, tempTask.ID), "no close job for a pending temp id")
	})

	t.Run("manual tasks repoint to the survivor", func(t *testing.T) {
		a := h.createContact(t, ctx, "task close survivor 3")
		b := h.createContact(t, ctx, "task close loser 3")
		// Both sides have a live manual task — no unique index covers
		// lifecycle='manual', so the repoint must be collision-free.
		aManual := h.seedTask(t, ctx, a.ID, contacttask.LifecycleManual, "managed", "tsk-man-a3", nil)
		bManual := h.seedTask(t, ctx, b.ID, contacttask.LifecycleManual, "managed", "tsk-man-b3", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		// B's manual task is USER CONTENT: still live, now on A, no close.
		h.requireTaskState(t, ctx, bManual.ID, repository.ContactTaskStateManaged, a.ID)
		h.requireTaskState(t, ctx, aManual.ID, repository.ContactTaskStateManaged, a.ID)
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, bManual.ID), "manual tasks are never remotely closed by merge")
	})

	t.Run("transfers live cadence row when target has no cadence row", func(t *testing.T) {
		a := h.createContact(t, ctx, "task transfer survivor 4")
		b := h.createContact(t, ctx, "task transfer loser 4")
		// A has no cadence_due row at all, so the transfer's state-agnostic
		// NOT EXISTS guard on unique_contact_provider_cadence is satisfied.
		bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleCadenceDue, "managed", "tsk-xfer-cad-b4", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateManaged, a.ID)
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, bTask.ID), "a transferred row is never closed")
	})

	t.Run("closes live cadence row when target holds a completed cadence row", func(t *testing.T) {
		a := h.createContact(t, ctx, "task transfer survivor 5")
		b := h.createContact(t, ctx, "task transfer loser 5")
		// A's cadence_due row is TERMINAL, but unique_contact_provider_cadence
		// has no state filter — the transfer guard sees ANY row and refuses.
		aTask := h.seedTask(t, ctx, a.ID, contacttask.LifecycleCadenceDue, "completed", "tsk-xfer-cad-a5", nil)
		bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleCadenceDue, "managed", "tsk-xfer-cad-b5", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateCompleted, b.ID)
		h.requireTaskState(t, ctx, aTask.ID, repository.ContactTaskStateCompleted, a.ID)
		assert.EqualValues(t, 1, h.closeJobCount(t, ctx, bTask.ID), "blocked transfer falls back to the close path")
	})

	t.Run("transfers live follow-up when target holds only a terminal follow-up", func(t *testing.T) {
		a := h.createContact(t, ctx, "task transfer survivor 6")
		b := h.createContact(t, ctx, "task transfer loser 6")
		// A's follow-up is TERMINAL: idx_contact_task_followup_unique_live only
		// covers live states, so a terminal target row does not block transfer.
		aTask := h.seedTask(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "completed", "tsk-xfer-fu-a6", nil)
		bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-xfer-fu-b6", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateManaged, a.ID)
		h.requireTaskState(t, ctx, aTask.ID, repository.ContactTaskStateCompleted, a.ID)
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, bTask.ID), "a transferred row is never closed")
	})

	t.Run("does not transfer follow-up with a colliding idempotency key", func(t *testing.T) {
		a := h.createContact(t, ctx, "task transfer survivor 7")
		b := h.createContact(t, ctx, "task transfer loser 7")
		// A's follow-up is TERMINAL (would not block on the live-state guard
		// alone) but shares B's idempotency_key: the second NOT EXISTS must
		// still refuse the transfer. B ALSO carries an unrelated, cleanly
		// transferable cadence_due row in the SAME UPDATE statement — this
		// is what makes the guard's presence observable: the transfer query
		// is one statement per merge, so if the idempotency guard did not
		// exclude the colliding row from the row-set, the whole statement
		// would abort on the first constraint hit and the savepoint
		// fallback would sweep the innocent cadence row into the close path
		// too, even though nothing blocks it on its own.
		const key = "shared-idem-key-7"
		aTask := h.seedTaskWithIdempotencyKey(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "completed", "tsk-xfer-idem-a7", key)
		bFollowUp := h.seedTaskWithIdempotencyKey(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-xfer-idem-b7", key)
		bCadence := h.seedTask(t, ctx, b.ID, contacttask.LifecycleCadenceDue, "managed", "tsk-xfer-idem-cad-b7", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		h.requireTaskState(t, ctx, bFollowUp.ID, repository.ContactTaskStateCompleted, b.ID)
		h.requireTaskState(t, ctx, aTask.ID, repository.ContactTaskStateCompleted, a.ID)
		assert.EqualValues(t, 1, h.closeJobCount(t, ctx, bFollowUp.ID), "idempotency-blocked transfer falls back to the close path")

		// The unrelated cadence row must transfer cleanly, unaffected by its
		// sibling's idempotency collision in the same UPDATE statement.
		h.requireTaskState(t, ctx, bCadence.ID, repository.ContactTaskStateManaged, a.ID)
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, bCadence.ID), "the unblocked sibling row is never closed")
	})

	t.Run("no task row is deleted by a merge", func(t *testing.T) {
		a := h.createContact(t, ctx, "task transfer survivor 8")
		b := h.createContact(t, ctx, "task transfer loser 8")
		// A mix of transferable, blocked, and manual rows on the source —
		// the point is that every one of B's row ids must survive the merge
		// SOMEWHERE, whether it moved to A or stayed on B closed.
		h.seedTask(t, ctx, b.ID, contacttask.LifecycleCadenceDue, "managed", "tsk-nodelete-cad-8", nil)
		h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-nodelete-fu-8", nil)
		h.seedTask(t, ctx, b.ID, contacttask.LifecycleManual, "managed", "tsk-nodelete-man-8", nil)

		preIDs, err := h.taskRepo.ListContactTasksByContact(ctx, b.ID)
		require.NoError(t, err)
		require.Len(t, preIDs, 3, "fixture sanity: three rows seeded on the source")

		_, err = h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err)

		for _, pre := range preIDs {
			_, err := h.taskRepo.GetContactTask(ctx, pre.ID)
			require.NoError(t, err, "row %s must still exist after the merge, transferred or not", pre.ID)
		}
	})
}

// TestMergeContacts_EnqueuerNotWired_Errors pins the D9 wiring contract:
// enqueue-eligible refs with the setter never called is a wiring bug (error +
// rollback); setter called with remoteCloseEnabled=false (follow-up mode
// 'off') closes locally with no job.
func TestMergeContacts_EnqueuerNotWired_Errors(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	t.Run("setter never called errors and rolls back", func(t *testing.T) {
		t.Parallel()
		h := newMergeAttrHarnessWith(t, ctx, false, false)
		a := h.createContact(t, ctx, "enqueuer survivor 1")
		b := h.createContact(t, ctx, "enqueuer loser 1")
		// A holds a live follow-up so B's row below is blocked from
		// transfer and reaches the close path this subtest exercises.
		h.seedTask(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-wire-blocker-a1", nil)
		bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-wire-b1", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SetTaskCloseEnqueuer")

		// Whole merge rolled back: task still live on B, B still live.
		h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateManaged, b.ID)
		_, err = h.contactRepo.GetContact(ctx, b.ID)
		require.NoError(t, err, "merge tx rolled back, B must still be live")
	})

	t.Run("remote close disabled closes locally without a job", func(t *testing.T) {
		t.Parallel()
		h := newMergeAttrHarnessWith(t, ctx, true, false)
		a := h.createContact(t, ctx, "enqueuer survivor 2")
		b := h.createContact(t, ctx, "enqueuer loser 2")
		// A holds a live follow-up so B's row below is blocked from
		// transfer and reaches the close path this subtest exercises.
		h.seedTask(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-wire-blocker-a2", nil)
		bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-wire-b2", nil)

		_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		require.NoError(t, err, "mode 'off' must not make merge unusable")

		h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateCompleted, b.ID)
		assert.EqualValues(t, 0, h.closeJobCount(t, ctx, bTask.ID), "no close job in mode off")
	})
}

// TestMergeContacts_TransferRaceFallsBackToClose is the D4-5 proof: a
// concurrent insert on the target can win the race between the transfer
// query's NOT EXISTS guard and its UPDATE, raising a 23505 that the
// savepoint must absorb rather than let abort the whole merge. Sequenced
// green-after-code (not red-first) — before the transfer statement exists
// there is nothing for tx B to block, so a red here would be invented, not
// observed.
//
// Uses logger.SetOutput, process-global mutable state, so this test does
// NOT run t.Parallel() (see internal/logger/logger.go SetOutput doc).
func TestMergeContacts_TransferRaceFallsBackToClose(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)
	a := h.createContact(t, ctx, "race survivor")
	b := h.createContact(t, ctx, "race loser")
	bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleCadenceDue, "managed", "tsk-race-b", nil)

	var logBuf bytes.Buffer
	restore := logger.SetOutput(&logBuf)
	defer restore()

	// Tx B: insert a live cadence_due row for the TARGET, deliberately left
	// uncommitted. Under READ COMMITTED, tx A's NOT EXISTS guard cannot see
	// this row (not yet committed), so the guard passes and A's UPDATE then
	// blocks on unique_contact_provider_cadence, held by B.
	txB, err := h.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txB.Rollback(ctx) }()

	_, err = h.taskRepo.CreateContactTaskTx(ctx, txB, repository.CreateContactTaskRequest{
		ContactID:      a.ID,
		Provider:       todoist.SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: "tsk-race-a",
		State:          "managed",
	}, nil)
	require.NoError(t, err)

	// Read B's own backend pid ON tx B — a pool checkout is a different
	// backend and would identify the wrong session.
	bPID, err := db.New(txB).TestBackendPID(ctx)
	require.NoError(t, err)

	// Tx A: run the merge in a goroutine. It will block inside the
	// transfer's UPDATE once it reaches the lock B holds.
	mergeDone := make(chan error, 1)
	go func() {
		_, mergeErr := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
			SourceContactID: b.ID,
			TargetContactID: a.ID,
		})
		mergeDone <- mergeErr
	}()

	// Poll from a THIRD connection (off the pool, via h.database.Queries —
	// neither tx A nor tx B) until exactly one backend is blocked by B's
	// pid. Scoped to B's own backend so a parallel sibling test waiting on
	// an unrelated lock cannot satisfy the poll. A timeout here means A
	// never reached the lock, and everything after it would be theatre —
	// require.Eventually fails the test rather than falling through.
	require.Eventually(t, func() bool {
		n, countErr := h.database.Queries.TestCountBackendsBlockedBy(ctx, bPID)
		return countErr == nil && n == 1
	}, 5*time.Second, 10*time.Millisecond, "merge transaction never blocked on tx B's lock")

	require.NoError(t, txB.Commit(ctx))

	require.NoError(t, <-mergeDone, "the savepoint must absorb the 23505 and let the merge commit")

	// The transfer lost the race; the row fell back to the close path.
	h.requireTaskState(t, ctx, bTask.ID, repository.ContactTaskStateCompleted, b.ID)
	assert.EqualValues(t, 1, h.closeJobCount(t, ctx, bTask.ID), "blocked-by-race transfer falls back to close")

	logged := logBuf.String()
	assert.Contains(t, logged, "merge: automated task transfer lost a race; falling back to close")
	assert.Contains(t, logged, b.ID.String(), "source_contact_id must be logged")
	assert.Contains(t, logged, a.ID.String(), "target_contact_id must be logged")
	assert.Contains(t, logged, "unique_contact_provider_cadence", "constraint must be logged")
}

// TestMatchOrCreate_TombstonedCacheFallsThroughToSurvivor isolates the
// liveness guard from the merge repoint: a DIRECT soft-delete (no repoint)
// leaves the identity cache pointing at a tombstoned contact, and the guard
// must fall through to discovery — self-healing to the surviving holder of
// the handle, or unlinking when no live holder exists.
func TestMatchOrCreate_TombstonedCacheFallsThroughToSurvivor(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)

	t.Run("survivor found and cache self-heals", func(t *testing.T) {
		const phone = "+15550100003"
		normalized := identity.Normalize(phone, identity.IdentifierTypePhone)

		b := h.createContact(t, ctx, "guard deleted holder")
		h.addPhone(t, ctx, b.ID, phone)

		pre, err := h.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: phone,
			Type:          identity.IdentifierTypePhone,
			Source:        "messages",
		})
		require.NoError(t, err)
		require.NotNil(t, pre.ContactID)
		require.Equal(t, b.ID, *pre.ContactID)

		// A second live holder of the same handle, then delete B directly
		// (no merge, no repoint — the cache row still points at B).
		a := h.createContact(t, ctx, "guard surviving holder")
		h.addPhone(t, ctx, a.ID, phone)
		require.NoError(t, h.contactSvc.DeleteContact(ctx, b.ID))

		post, err := h.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: phone,
			Type:          identity.IdentifierTypePhone,
			Source:        "messages",
		})
		require.NoError(t, err)
		require.NotNil(t, post.ContactID)
		assert.Equal(t, a.ID, *post.ContactID, "guard falls through; discovery finds the only live holder")
		assert.False(t, post.Cached, "tombstoned cache hit must not short-circuit")

		// The identity row itself self-healed to A.
		ident, err := h.identityRepo.GetByIdentifier(ctx, identity.IdentifierTypePhone, normalized, "messages")
		require.NoError(t, err)
		require.NotNil(t, ident.ContactID)
		assert.Equal(t, a.ID, *ident.ContactID)
	})

	t.Run("no survivor unlinks the identity", func(t *testing.T) {
		const phone = "+15550100004"
		normalized := identity.Normalize(phone, identity.IdentifierTypePhone)

		c := h.createContact(t, ctx, "guard sole holder")
		h.addPhone(t, ctx, c.ID, phone)

		pre, err := h.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: phone,
			Type:          identity.IdentifierTypePhone,
			Source:        "messages",
		})
		require.NoError(t, err)
		require.NotNil(t, pre.ContactID)
		require.Equal(t, c.ID, *pre.ContactID)

		require.NoError(t, h.contactSvc.DeleteContact(ctx, c.ID))

		post, err := h.identitySvc.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: phone,
			Type:          identity.IdentifierTypePhone,
			Source:        "messages",
		})
		require.NoError(t, err)
		assert.Nil(t, post.ContactID, "no live holder — unmatched")
		assert.Equal(t, repository.MatchTypeUnmatched, post.MatchType)

		// The stale link was explicitly cleared (COALESCE in the upsert
		// would otherwise have preserved the dead contact_id), so the row
		// surfaces in the unmatched queue.
		ident, err := h.identityRepo.GetByIdentifier(ctx, identity.IdentifierTypePhone, normalized, "messages")
		require.NoError(t, err)
		assert.Nil(t, ident.ContactID, "identity unlinked from the tombstoned contact")
	})
}

// mergeAdapterMockTodoist records Sync calls for the adapter-through-
// live-site test.
type mergeAdapterMockTodoist struct {
	commands []todoist.SyncCommand
}

func (m *mergeAdapterMockTodoist) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, assertErr("QuickAdd not used")
}

func (m *mergeAdapterMockTodoist) Sync(_ context.Context, _ string, _ []string, cmds []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	m.commands = append(m.commands, cmds...)
	return &todoist.SyncResponse{}, nil
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

// TestMergeContacts_CloseEnqueue_ExecutesThroughAdapter is the binding
// gate for the reconciler PR1 cutover: the merge-close call site keeps
// enqueuing the LEGACY todoist_followup_close kind, and after the bespoke
// close worker is deleted the surviving legacy job must still execute —
// via the transitional close adapter delegating to the unified executor.
// Drives the live merge site, then runs the enqueued legacy-kind job
// through the adapter and asserts it reaches an item_close for the row's
// real external id.
func TestMergeContacts_CloseEnqueue_ExecutesThroughAdapter(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := ctx0()

	h := newMergeAttrHarness(t, ctx)
	a := h.createContact(t, ctx, "adapter survivor")
	b := h.createContact(t, ctx, "adapter loser")
	// A holds a live follow-up so B's row below is blocked from transfer
	// and reaches the close path — this test is about the close path's
	// adapter execution, not the transfer guard.
	h.seedTask(t, ctx, a.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-adapter-blocker-a", nil)
	// B carries a real-external-id follow-up so the merge enqueues a close.
	bTask := h.seedTask(t, ctx, b.ID, contacttask.LifecycleFollowUpLoop, "managed", "tsk-adapter-b", nil)

	_, err := h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{
		SourceContactID: b.ID,
		TargetContactID: a.ID,
	})
	require.NoError(t, err)

	// The live site enqueued exactly one LEGACY-kind close job.
	require.EqualValues(t, 1, h.closeJobCount(t, ctx, bTask.ID),
		"merge close must enqueue a legacy todoist_followup_close job")

	// Now execute that legacy-kind job through the close adapter →
	// unified executor → item_close.
	mock := &mergeAdapterMockTodoist{}
	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{ProjectID: "p", LabelName: "l", IntegrationInstanceID: "i"}, "token", nil
	}
	executor := consumer.NewTodoistTaskOpWorker(
		consumer.FollowUpModeCutover,
		h.taskRepo, settings, func(string) todoist.Client { return mock }, nil, h.database.Pool,
	)
	adapter := consumer.NewTodoistFollowUpCloseAdapterWorker(executor)
	require.NoError(t, adapter.Work(ctx, &river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]{
		Args: consumerjobs.TodoistFollowUpCloseJobArgs{ContactTaskID: bTask.ID},
	}))

	require.Len(t, mock.commands, 1)
	assert.Equal(t, "item_close", mock.commands[0].Type)
	assert.Equal(t, "tsk-adapter-b", mock.commands[0].Args["id"],
		"adapter must close the row's real external id")
}
