package api

import (
	"context"
	"testing"

	"personal-crm/backend/internal/anarlog"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// newIngestDeps returns a complete IngestDeps over real repositories and
// services on database, publishing through bus. A test replaces a field only
// to fake what it cannot run or to inject a failure.
func newIngestDeps(t *testing.T, database *db.Database, bus *events.Bus) service.IngestDeps {
	t.Helper()
	cfg := config.TestConfig()

	// Insert-only river client: the inline call.* path enqueues follow-up and
	// cadence jobs; the shims accept the kinds and never run.
	workers := river.NewWorkers()
	river.AddWorker(workers, &apiTestCadenceShim{})
	river.AddWorker(workers, &apiTestFollowUpShim{})
	river.AddWorker(workers, &apiKnowledgeCacheNoopWorker{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	phoneCallRepo := repository.NewPhoneCallRepository(database.Queries)

	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover, false, cfg.Watchdog,
	)
	followUpManager := consumer.NewFollowUpManager(
		consumer.FollowUpModeCutover,
		claimRepo, contactRepo, contactTaskRepo, contactTaskRepo, interactionRepo,
		riverClient,
		func(context.Context) (*todoist.Settings, string, error) {
			return nil, "", consumer.ErrTodoistUnconfigured
		},
		cfg.CORS.FrontendURL,
		cfg.Watchdog,
	)
	assertSvc, knowledgeCache := buildKnowledgeDepsForAPITest(t, database, bus)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, bus, nil,
		cadenceUpdater, assertSvc, knowledgeCache, followUpManager)

	enrichmentService := service.NewEnrichmentService(database, contactRepo, contactMethodRepo,
		repository.NewEnrichmentRepository(database.Queries), nil, nil, nil, nil, nil)

	return service.IngestDeps{
		Database:              database,
		Bus:                   bus,
		Identity:              service.NewIdentityService(identityRepo),
		Messages:              repository.NewMessagesMessageRepository(database.Queries),
		RiverClient:           riverClient,
		ExternalContacts:      externalRepo,
		HostLiveness:          repository.NewMacHostRepository(database.Queries),
		MeetingNotes:          repository.NewMeetingNoteRepository(database.Queries),
		Calendar:              calendarRepo,
		PhoneCallLinkage:      phoneCallRepo,
		Interactions:          interactionRepo,
		IdentityLookup:        identityRepo,
		ContactSvc:            contactService,
		TitleMatcher:          anarlog.NewTitleMatcher(contactRepo),
		Discovery:             anarlog.NewDiscoveryWriter(externalRepo),
		PhoneCalls:            phoneCallRepo,
		ContactRecorder:       contactService,
		Cadence:               cadenceUpdater,
		FollowUp:              followUpManager,
		AddressBookReconciler: service.NewAddressBookReconcileService(enrichmentService, contactRepo, contactMethodRepo, externalRepo),
		Venue: repository.NewVenueResolverRegistry(
			repository.NewVenueRepository(database.Queries), nil, calendarRepo),
	}
}
