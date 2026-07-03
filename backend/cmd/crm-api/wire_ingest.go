package main

import (
	"personal-crm/backend/internal/anarlog"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/service"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// ingestStack holds the IngestService (its AddressBookReconciler is wired
// later, in buildDomainServices) plus the ingest + meeting-note handlers
// consumed by route registration.
type ingestStack struct {
	IngestService      *service.IngestService
	IngestHandler      *handlers.IngestHandler
	MeetingNoteHandler *handlers.MeetingNoteHandler
}

// buildIngestStack constructs the IngestService (hoisted after the
// consumers so the call.* inline handler can reuse contactService +
// cadenceUpdater + followUpManager + eventBus to emit interaction.recorded
// and apply cadence/follow-up in the same tx as the phone_call staging-row
// write) and the meeting-note conflict-resolution surface. The
// AddressBookReconciler back-reference is set later in buildDomainServices.
func buildIngestStack(
	database *db.Database,
	core coreRepos,
	graph contactCore,
	ingest ingestRepos,
	messaging messagingFoundation,
	consumers eventConsumers,
	eventBus *events.Bus,
	riverClient *river.Client[pgx.Tx],
) ingestStack {
	contactRepo := core.Contact
	interactionRepo := core.Interaction
	contactService := graph.ContactService
	identityServiceForIngest := ingest.IdentityService
	messagesMessageRepo := ingest.MessagesMessage
	externalContactRepoForIngest := ingest.ExternalContact
	macHostRepoForIngest := ingest.MacHost
	meetingNoteRepoForIngest := ingest.MeetingNote
	calendarRepoForIngest := ingest.CalendarEvent
	phoneCallRepoForIngest := ingest.PhoneCall
	identityRepoForIngest := ingest.Identity
	cadenceUpdater := consumers.CadenceUpdater
	followUpManager := consumers.FollowUpManager
	venueResolver := messaging.VenueResolver

	// IngestService — hoisted here so the call.* inline handler can
	// reuse contactService.RecordInteractionTx, cadenceUpdater,
	// followUpManager, and eventBus.PublishTx to emit
	// interaction.recorded + apply cadence + follow-up in the SAME tx
	// as the phone_call staging row write (spec §`phone_calls`
	// content-delivered cadence). raw_message.* and external_contact.*
	// inline handlers don't need these four — they were nil before the
	// v1.5 expansion. The meeting_note.* inline handler reuses
	// contactService via the ContactInteractionRecorder interface for
	// session-attributed interaction writes so cadence + follow-up
	// fire correctly.
	//
	// anarlog title-extraction deps for the meeting_note.recorded inline
	// handler: TitleMatcher disambiguates a single name token against
	// the CRM contact table (trigram + collision-gap); DiscoveryWriter
	// persists weak-candidate anarlog_title rows for unmatched tokens.
	titleMatcher := anarlog.NewTitleMatcher(contactRepo)
	titleDiscoveryWriter := anarlog.NewDiscoveryWriter(externalContactRepoForIngest)
	ingestService := service.NewIngestService(
		database,
		eventBus,
		identityServiceForIngest,
		messagesMessageRepo,
		riverClient,
		externalContactRepoForIngest,
		macHostRepoForIngest, // host-liveness re-check inside the batch tx
		meetingNoteRepoForIngest,
		calendarRepoForIngest,
		interactionRepo,
		identityRepoForIngest,
		contactService,
		phoneCallRepoForIngest,
		contactService,
		cadenceUpdater,
		followUpManager,
		titleMatcher,
		titleDiscoveryWriter,
		phoneCallRepoForIngest, // phone_call linkage candidates for meeting_note Step 1
	)
	// Populate interaction.venue_id for phone_calls + anarlog_sessions
	// interactions the ingest inline handlers write.
	ingestService.SetVenueResolver(venueResolver)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	// User-driven conflict-resolution surface for meeting_note rows.
	// Mirrors the daemon-side IngestService.handleMeetingNoteRecorded
	// path; uses a small adapter to satisfy the polymorphic
	// LinkageTargetReader interface (event vs phone_call lookup by UUID).
	meetingNoteLinkageTargets := service.NewLinkageTargetReader(calendarRepoForIngest, phoneCallRepoForIngest)
	meetingNoteService := service.NewMeetingNoteService(
		database,
		meetingNoteRepoForIngest,
		meetingNoteRepoForIngest,
		meetingNoteLinkageTargets,
		identityRepoForIngest,
		titleMatcher,
		titleDiscoveryWriter,
		contactService,
		contactRepo,
	)
	// Populate venue_id on the resolve-link interaction path.
	meetingNoteService.SetVenueResolver(venueResolver)
	meetingNoteHandler := handlers.NewMeetingNoteHandler(meetingNoteService)

	return ingestStack{
		IngestService:      ingestService,
		IngestHandler:      ingestHandler,
		MeetingNoteHandler: meetingNoteHandler,
	}
}
