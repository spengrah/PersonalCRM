//go:build integration_testdb

package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func newMergeAttrHarness(t *testing.T, ctx context.Context) *mergeAttrHarness {
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
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, eventRepo)

	assertSvc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)
	cacheUpdater := consumer.NewKnowledgeCacheUpdater(assertionRepo, nodeRepo, contactRepo)

	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil)
	contactSvc.SetKnowledgeWriter(assertSvc, cacheUpdater)
	wireCadenceUpdaterForTest(t, database, contactSvc)

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
