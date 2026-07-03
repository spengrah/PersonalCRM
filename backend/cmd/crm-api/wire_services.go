package main

import (
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/service"
)

// domainServices holds the note / import-match / enrichment /
// address-book-reconcile services built at outer scope so the feature
// blocks (external sync, telegram) share single instances.
type domainServices struct {
	NoteService                 *service.NoteService
	ImportMatchService          *service.ImportMatchService
	EnrichmentService           *service.EnrichmentService
	AddressBookReconcileService *service.AddressBookReconcileService
}

// buildDomainServices constructs the note, import-match, enrichment, and
// address-book-reconcile services and wires the EnrichmentService setters
// (cadence + knowledge writer) plus the IngestService's AddressBookReconciler
// back-reference. EnrichmentService is shared by the import handler and the
// Telegram peer matcher, so it is built once here.
func buildDomainServices(
	database *db.Database,
	core coreRepos,
	graph contactCore,
	ingest ingestRepos,
	consumers eventConsumers,
	ingestStk ingestStack,
	eventBus *events.Bus,
) domainServices {
	noteRepo := core.Note
	contactRepo := core.Contact
	contactMethodRepo := core.ContactMethod
	enrichmentRepo := core.Enrichment
	rematchService := graph.RematchService
	assertService := graph.AssertService
	cadenceUpdater := consumers.CadenceUpdater
	knowledgeCacheUpdater := consumers.KnowledgeCacheUpdater
	externalContactRepoForIngest := ingest.ExternalContact
	ingestService := ingestStk.IngestService

	noteService := service.NewNoteService(noteRepo, contactRepo)
	importMatchService := service.NewImportMatchService(contactRepo)
	// EnrichmentService is shared by the import handler (link/import flows) and
	// the Telegram peer matcher (auto-match enrichment). Constructed at outer
	// scope so both feature blocks share a single instance.
	enrichmentService := service.NewEnrichmentService(database, contactRepo, contactMethodRepo, enrichmentRepo, eventBus, rematchService)
	enrichmentService.SetCadenceUpdater(cadenceUpdater)
	// Inferred location/birthday from external contact data flow through the
	// assertion store (agent provenance), not the contact SQL — wire the same
	// knowledge writer the contact service uses.
	enrichmentService.SetKnowledgeWriter(assertService, knowledgeCacheUpdater)

	// Address-book method reconcile: re-propagates address-book methods
	// onto already-linked contacts (auto-propagate for matched, record
	// suggestion for imported). Shared by the gcontacts forward hook and
	// the icloud post-commit hook so the auto-vs-suggest + dup precedence
	// logic lives in one place.
	addressBookReconcileService := service.NewAddressBookReconcileService(
		enrichmentService,
		contactRepo,
		contactMethodRepo,
		externalContactRepoForIngest,
	)
	ingestService.SetAddressBookReconciler(addressBookReconcileService)

	return domainServices{
		NoteService:                 noteService,
		ImportMatchService:          importMatchService,
		EnrichmentService:           enrichmentService,
		AddressBookReconcileService: addressBookReconcileService,
	}
}
